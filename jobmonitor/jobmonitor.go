/*
 * Copyright 2018. IBM Corporation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package jobmonitor

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-kit/kit/metrics/statsd"

	"github.com/cenkalti/backoff"
	"github.com/go-kit/kit/metrics"

	"google.golang.org/grpc"

	"github.com/AISphere/ffdl-commons/config"
	"github.com/AISphere/ffdl-lcm/coord"
	"github.com/AISphere/ffdl-lcm/lcmconfig"

	"github.com/AISphere/ffdl-commons/logger"

	"k8s.io/client-go/kubernetes"

	service "github.com/AISphere/ffdl-lcm/service"
	lcmClient "github.com/AISphere/ffdl-lcm/service/client"
	"github.com/AISphere/ffdl-trainer/client"
	"github.com/AISphere/ffdl-trainer/trainer/grpc_trainer_v2"
)

// Confuse `go vet' to not check this `Errorf' call. :(
// See https://github.com/grpc/grpc-go/issues/90
var gerrf = grpc.Errorf

const (
	zkLearners = "learners"
	zkLearner  = "learner_"
	zkStatus   = "status"
)

const (
	numRetries             = 10
	insuffResourcesRetries = 10
	ctxTimeout             = 10 * time.Second
)

type jobMonitorMetrics struct {
	failedETCDConnectivityCounter, failedK8sConnectivityCounter, insufficientK8sResourcesErrorCounter, failedImagePullK8sErrorCounter,
	failedETCDWatchCounter metrics.Counter
}

//JobMonitor ...
type JobMonitor struct {
	k8sClient             kubernetes.Interface
	UseNativeDistribution bool
	TrainingID            string
	UserID                string
	JobName               string
	NumLearners           int
	trMap                 map[string]([]string)
	numTerminalLearners   uint64
	metrics               *jobMonitorMetrics
	EtcdClient            coord.Coordinator
}

var failedTrainerConnectivityCounter metrics.Counter

// count etcd progress notifications (arrive every 10 mins)
var etcdJobProgressNotificationCounter uint32
var etcdLearnerProgressNotificationCounter uint32

// number of progress notifications to count before dropping a log line (e.g., 6 * 10 minutes = log every hour)
const etcdProgressNotificationLogFrequency = 6

//NewJobMonitor ...
func NewJobMonitor(trainingID string, userID string, numLearners int, jobName string, useNativeDistribution bool, statsdClient *statsd.Statsd, logr *logger.LocLoggingEntry) (*JobMonitor, error) {

	logr.Infof("Starting Job Monitor service for training %s", trainingID)
	// assert necessary config keys
	config.FatalOnAbsentKey(config.ETCDEndpoints)

	jmMetrics := jobMonitorMetrics{
		failedETCDConnectivityCounter:        statsdClient.NewCounter("jobmonitor.etcd.connectivity.failed", 1),
		failedK8sConnectivityCounter:         statsdClient.NewCounter("jobmonitor.k8s.connectivity.failed", 1),
		insufficientK8sResourcesErrorCounter: statsdClient.NewCounter("jobmonitor.k8s.insufficientResources.failed", 1),
		failedImagePullK8sErrorCounter:       statsdClient.NewCounter("jobmonitor.k8s.imagePull.failed", 1),
		failedETCDWatchCounter:               statsdClient.NewCounter("jobmonitor.etcd.watch.failed", 1),
	}

	k8sConfig, err := lcmconfig.GetKubernetesConfig()
	if err != nil {
		logr.WithError(err).Errorf("Failed to obtain kubernetes config for jobmonitor: %v", k8sConfig)
		return nil, err
	}

	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		jmMetrics.failedK8sConnectivityCounter.Add(1)
		logr.WithError(err).Errorf("Failed to connect to k8s while creating new lcm service for training %s", trainingID)

		if err := updateJobStatusOnError(trainingID, userID, client.ErrCodeK8SConnection, service.StatusMessages_INTERNAL_ERROR.String(), logr); err != nil {
			logr.WithError(err).Errorf("Failed to write the status %s for training %s to trainer", grpc_trainer_v2.Status_FAILED, trainingID)
		}
		if err := KillDeployedJob(trainingID, userID, jobName, logr); err != nil {
			logr.WithError(err).Errorf("Failed to kill the deployed job %s", trainingID)
		}
		return nil, fmt.Errorf("Failed to connect to k8s")
	}

	client, connectivityErr := coordinator(logr)
	if connectivityErr != nil {
		shutdownTrainingOnETCDFailure(trainingID, userID, jobName, connectivityErr, logr)
		return nil, connectivityErr
	}

	jm := &JobMonitor{
		k8sClient:             k8sClient,
		UseNativeDistribution: useNativeDistribution,
		TrainingID:            trainingID,
		UserID:                userID,
		JobName:               jobName,
		NumLearners:           numLearners,
		trMap:                 initTransitionMap(),
		metrics:               &jmMetrics,
		EtcdClient:            client,
	}

	return jm, nil
}

//update job status in mongo
func updateJobStatusInTrainer(trainingID string, userID string, statusUpdate *client.TrainingStatusUpdate, logr *logger.LocLoggingEntry) error {
	updStatus := statusUpdate.Status
	logr.Infof("(updateJobStatus) Updating status of %s to %s", trainingID, updStatus.String())
	updateRequest := &grpc_trainer_v2.UpdateRequest{TrainingId: trainingID, Status: updStatus, Timestamp: statusUpdate.Timestamp,
		UserId: userID, StatusMessage: statusUpdate.StatusMessage, ErrorCode: statusUpdate.ErrorCode}
	trainer, err := client.NewTrainer()
	if err != nil {
		logr.WithError(err).Errorf("(updateJobStatus) Creating training client for status update failed. Training ID %s New Status %s", trainingID, updStatus.String())
	}
	defer trainer.Close()

	defaultBackoff := backoff.NewExponentialBackOff()
	defaultBackoff.MaxElapsedTime = 1 * time.Minute
	defaultBackoff.MaxInterval = 5 * time.Second

	err = backoff.RetryNotify(func() error {
		_, err = trainer.Client().UpdateTrainingJob(context.Background(), updateRequest)
		return err
	}, defaultBackoff, func(err error, t time.Duration) {
		logr.WithError(err).Errorf("Failed to update status to the trainer. Retrying WARNING: Status updates for %s may be temporarily inconsistent due to failure to communicate with Trainer.", trainingID)
	})

	if err != nil {
		failedTrainerConnectivityCounter.Add(1)
		logr.WithError(err).Errorf("Failed to update status to the trainer. Already retried several times.WARNING : Status of job %s will likely be incorrect", trainingID)
		return err
	}

	return err
}

// update job status in mongo on error
func updateJobStatusOnError(trainingID string, userID string, errorCode string, statusMessage string, logr *logger.LocLoggingEntry) error {
	statusUpdate := client.TrainingStatusUpdate{
		Status:        grpc_trainer_v2.Status_FAILED,
		Timestamp:     client.CurrentTimestampAsString(),
		ErrorCode:     errorCode,
		StatusMessage: statusMessage,
	}
	return updateJobStatusInTrainer(trainingID, userID, &statusUpdate, logr)
}

//ManageDistributedJob ...manages a DLaaS training job
func (jm *JobMonitor) ManageDistributedJob(logr *logger.LocLoggingEntry) {
	go jm.checkIfJobStarted(logr)
	go jm.monitorJob(logr)
}

//monitors the job at the path jobBasePath() generall /training_id/ under which there is /training_id/status/ indicating over all job status
//and there can be jobLearnerStatusPath() generally /training_id/learners/learner_1/status/ , 2 and 3 indicating status of individual learners
//the trailing slash on status/ on learner is important as it distinguishes the regex from status_summary_metrics
func (jm *JobMonitor) monitorJob(logr *logger.LocLoggingEntry) {

	err := backoff.RetryNotify(func() error {
		_, err := jm.EtcdClient.PutIfKeyMissing(overallJobStatusPath(jm.TrainingID), grpc_trainer_v2.Status_NOT_STARTED.String(), logr)
		return err
	}, etdInteractionBackoff(1*time.Minute, 10*time.Second), func(err error, t time.Duration) { jm.metrics.failedETCDConnectivityCounter.Add(1) })

	//not doing anything here, since this is probably a job monitor restarting
	if err != nil {
		logr.WithError(err).Warnf("job monitor possibly restarted and that's why the status %s for the path %s :", grpc_trainer_v2.Status_NOT_STARTED.String(), overallJobStatusPath(jm.TrainingID))
	}

	//processed[1], for example, stores the number of status updates of learner 1 that have been processed
	processed := make(map[int]int)

	for i := 1; i <= jm.NumLearners; i++ {
		//To start, no status updates have been processed for any learner
		processed[i] = 0
	}

	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {

		for i := 1; i <= jm.NumLearners; i++ {
			seqName := indvidualJobStatusPath(jm.TrainingID, i)
			seq := jm.EtcdClient.NewValueSequence(seqName, logr)
			statuses, err := seq.GetAll(logr)

			if err != nil {
				logr.Errorf("Job Monitor could not connect to ETCD to get the status of Learner %d\n", i)
				jm.metrics.failedETCDConnectivityCounter.Add(1)
				continue
			}

			for j := processed[i]; j < len(statuses); j++ {
				jm.processUpdateLearnerStatus(seqName, statuses[j], logr)
				processed[i]++
			}
		}
	}

}

//gets triggered when the /status node is updated
//This function updates the overall job status with trainer and calls LCM to clean up the job when necessary
//This function should only return true if the job needs no further status monitoring
func (jm *JobMonitor) processUpdateJobStatus(currStatus string, logr *logger.LocLoggingEntry) bool {
	logr.Infof("(processUpdateJobStatus) got triggered with the current status %s", currStatus)
	//Variable to notify whether the job needs further status monitoring
	markComplete := false
	statusUpdate := client.GetStatus(currStatus, logr)

	status := statusUpdate.Status
	error := updateJobStatusInTrainer(jm.TrainingID, jm.UserID, statusUpdate, logr)
	if error != nil {
		logr.WithError(error).Errorf("Failed to write the status %s for training %s to trainer", status, jm.TrainingID)
	}

	//if native distribution and status of the entire job is complete then kill the deployed job
	if status == grpc_trainer_v2.Status_COMPLETED || status == grpc_trainer_v2.Status_FAILED || status == grpc_trainer_v2.Status_HALTED {
		logr.Infof("(processUpdateJobStatus) overall status of the job was set up as %v and native distribution status was %v", currStatus, jm.UseNativeDistribution)
		if jm.UseNativeDistribution {
			logr.Debugf("(processUpdateJobStatus) No need to wait for all learners to terminate. Already updated status. Killing job %s", jm.TrainingID)
			err := KillDeployedJob(jm.TrainingID, jm.UserID, jm.JobName, logr)
			if err != nil {
				logr.WithError(err).Errorf("(processUpdateJobStatus) failed to kill the deployed job %s", jm.TrainingID)
			}
			markComplete = true
			return markComplete
		}
		//Job has completed, now wait 1 minute for all learners to upload logs and clean themselves up
		if atomic.LoadUint64(&jm.numTerminalLearners) < uint64(jm.NumLearners) {
			logr.Debugf("(processUpdateJobStatus) Sleeping for 60s to allow all remaining learners to complete")
			time.Sleep(60 * time.Second)
		}
		// check if they cleaned themselves up, and log it.  Teardown happens either way.
		if atomic.LoadUint64(&jm.numTerminalLearners) < uint64(jm.NumLearners) {
			logr.Debugf("(processUpdateJobStatus) Killing remaining learners in %s", jm.TrainingID)
		} else {
			logr.Debugf("(processUpdateJobStatus) All learners of %s have completed. It can now be safely killed", jm.TrainingID)
		}
		err := KillDeployedJob(jm.TrainingID, jm.UserID, jm.JobName, logr)
		if err != nil {
			logr.WithError(err).Errorf("(processUpdateJobStatus) failed to kill the deployed job %s", jm.TrainingID)
		}
		markComplete = true
	}

	return markComplete
}

//This function processes an update to learner status, i.e. it updates the overall job status
func (jm *JobMonitor) processUpdateLearnerStatus(learnerStatusPath string, learnerStatusValue string, logr *logger.LocLoggingEntry) error {

	learnerStatus := client.GetStatus(learnerStatusValue, logr).Status
	logr.Infof("got triggered with the current path %s and value %s (status %s)", learnerStatusPath, learnerStatusValue, learnerStatus)

	response, err := jm.EtcdClient.Get(overallJobStatusPath(jm.TrainingID), logr)
	if err != nil {
		return err
	}

	if response == nil || len(response) == 0 {
		return fmt.Errorf(" while processing update from learner, the value at overall job status path %s was empty, the default value is NOT_STARTED", overallJobStatusPath(jm.TrainingID))
	}

	currentOverallJobStatus := response[0].Value
	// currentOverallJobStatus may be a JSON value -> parse and convert to TrainingStatusUpdate struct
	currentOverallJobStatusObj := client.GetStatus(currentOverallJobStatus, logr)
	jobStatus := currentOverallJobStatusObj.Status
	if jm.isTransitionAllowed(jobStatus.String(), learnerStatus.String()) {
		logr.Infof("Transition was allowed, changing overall status of job from %s to learners status %s", jobStatus, learnerStatus)
		jm.EtcdClient.CompareAndSwap(overallJobStatusPath(jm.TrainingID), learnerStatusValue, currentOverallJobStatus, logr)
		jm.processUpdateJobStatus(learnerStatusValue, logr)
	} else {
		logr.Warnf("Transition not allowed job from overall job status %s to learner status %s", jobStatus, learnerStatus)
	}
	//keep an eye on idividual learners as well, if they terminate then check if all of them are done then check if job can be terminated
	if learnerStatus == grpc_trainer_v2.Status_COMPLETED || learnerStatus == grpc_trainer_v2.Status_FAILED || learnerStatus == grpc_trainer_v2.Status_HALTED {
		atomic.AddUint64(&jm.numTerminalLearners, 1)
	}
	return err
}

func overallJobStatusPath(trainingID string) string {
	return trainingID + "/" + zkStatus
}

func indvidualJobStatusPath(trainingID string, learnerNum int) string {
	return fmt.Sprintf("%s/%s/%s%d/%s/", trainingID, zkLearners, zkLearner, learnerNum, zkStatus)
}

func jobBasePath(trainingID string) string {
	return trainingID + "/"
}

//KillDeployedJob ... Contact the LCM and kill training job
func KillDeployedJob(trainingID string, userID string, jobName string, logr *logger.LocLoggingEntry) error {
	time.Sleep(10 * time.Second)
	logr.Infof("(killDeployedJob) Sending job kill request to LCM for %s", trainingID)
	jobKillReq := &service.JobKillRequest{Name: jobName, TrainingId: trainingID, UserId: userID}
	lcm, err := lcmClient.NewLcm(nil)
	if err != nil {
		logr.Errorln("(KillDeployedJob) Cannot create lcm service client: ", err.Error())
		return err
	}
	defer lcm.Close()

	defaultBackoff := backoff.NewExponentialBackOff()
	defaultBackoff.MaxElapsedTime = 1 * time.Minute
	defaultBackoff.MaxInterval = 5 * time.Second

	err = backoff.Retry(func() error {
		_, err = lcm.Client().KillTrainingJob(context.Background(), jobKillReq)
		if err != nil {
			logr.WithError(err).Errorf("Failed to send request to LCM to garbage collect Training Job %s. Retrying", trainingID)
		}
		return err
	}, defaultBackoff)

	if err != nil {
		logr.WithError(err).Errorf("(killDeployedJob) Successfully sent request to LCM to garbage collect Failed to send request to LCM to garbage collect Training Job %s. Already retried several times.", trainingID)
		return err
	}

	return err
}

func learnerSummaryMetricsPath(trainingID string, learnerID int) string {
	return fmt.Sprintf("%s/learners/learner_%d/%s", trainingID, learnerID, "summary_metrics")
}

func initTransitionMap() map[string]([]string) {
	transistionMap := make(map[string]([]string))
	allowDOWNLOADING := []string{grpc_trainer_v2.Status_PENDING.String(), grpc_trainer_v2.Status_NOT_STARTED.String()}
	allowPROCESSING := []string{grpc_trainer_v2.Status_PROCESSING.String(), grpc_trainer_v2.Status_DOWNLOADING.String(), grpc_trainer_v2.Status_PENDING.String()}
	allowSTORING := []string{grpc_trainer_v2.Status_PROCESSING.String(), grpc_trainer_v2.Status_DOWNLOADING.String(), grpc_trainer_v2.Status_PENDING.String(), grpc_trainer_v2.Status_NOT_STARTED.String()}
	allowCOMPLETED := []string{grpc_trainer_v2.Status_STORING.String(), grpc_trainer_v2.Status_PROCESSING.String(), grpc_trainer_v2.Status_DOWNLOADING.String(), grpc_trainer_v2.Status_PENDING.String(), grpc_trainer_v2.Status_NOT_STARTED.String()}
	allowFAILED := []string{grpc_trainer_v2.Status_STORING.String(), grpc_trainer_v2.Status_PROCESSING.String(), grpc_trainer_v2.Status_DOWNLOADING.String(), grpc_trainer_v2.Status_PENDING.String(), grpc_trainer_v2.Status_NOT_STARTED.String()}
	allowHALTED := []string{grpc_trainer_v2.Status_STORING.String(), grpc_trainer_v2.Status_PROCESSING.String(), grpc_trainer_v2.Status_DOWNLOADING.String(), grpc_trainer_v2.Status_PENDING.String(), grpc_trainer_v2.Status_NOT_STARTED.String()}

	transistionMap[grpc_trainer_v2.Status_DOWNLOADING.String()] = allowDOWNLOADING
	transistionMap[grpc_trainer_v2.Status_PROCESSING.String()] = allowPROCESSING
	transistionMap[grpc_trainer_v2.Status_STORING.String()] = allowSTORING
	transistionMap[grpc_trainer_v2.Status_COMPLETED.String()] = allowCOMPLETED
	transistionMap[grpc_trainer_v2.Status_FAILED.String()] = allowFAILED
	transistionMap[grpc_trainer_v2.Status_HALTED.String()] = allowHALTED
	return transistionMap
}

func (jm *JobMonitor) isTransitionAllowed(fromStatus string, toStatus string) bool {
	validFroms := jm.trMap[toStatus]
	for _, allowed := range validFroms {
		if fromStatus == allowed {
			return true
		}
	}
	return false
}

func etdInteractionBackoff(maxElapsedTime, maxInterval time.Duration) *backoff.ExponentialBackOff {
	back := backoff.NewExponentialBackOff()
	back.MaxElapsedTime = maxElapsedTime
	back.MaxInterval = maxInterval
	return back
}

//onError function on how to deal with the scenario if connecting to coordinator failed. the error is still returned in case
func coordinator(logr *logger.LocLoggingEntry) (coord.Coordinator, error) {

	var instance coord.Coordinator
	var err error
	err = backoff.
		RetryNotify(func() error {
			instance, err = coord.NewCoordinator(coord.Config{Endpoints: config.GetEtcdEndpoints(), Prefix: config.GetEtcdPrefix(),
				Cert: config.GetEtcdCertLocation(), Username: config.GetEtcdUsername(), Password: config.GetEtcdPassword()}, logr)
			return err
		}, etdInteractionBackoff(1*time.Minute, 30*time.Second), func(err error, t time.Duration) {
			logr.WithError(err).Errorf("failed to establish connection with etcd")
		})

	return instance, err
}

func shutdownTrainingOnETCDFailure(trainingID, userID, jobName string, err error, logr *logger.LocLoggingEntry) {

	logr.WithError(err).Error("failed to connect to etcd while monitoring training and shutting down the job")
	if err := updateJobStatusOnError(trainingID, userID, client.ErrCodeEtcdConnection, service.StatusMessages_INTERNAL_ERROR.String(), logr); err != nil {
		logr.WithError(err).Errorf("Failed to write the status %s for training %s to trainer", grpc_trainer_v2.Status_FAILED, trainingID)
	}
	if err := KillDeployedJob(trainingID, userID, jobName, logr); err != nil {
		logr.WithError(err).Errorf("Failed to kill the deployed job %s", trainingID)
	}
}
