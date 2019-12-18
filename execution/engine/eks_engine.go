package engine

import (
	"encoding/base64"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/pkg/errors"
	"github.com/stitchfix/flotilla-os/clients/metrics"
	"github.com/stitchfix/flotilla-os/config"
	"github.com/stitchfix/flotilla-os/execution/adapter"
	flotillaLog "github.com/stitchfix/flotilla-os/log"
	"github.com/stitchfix/flotilla-os/queue"
	"github.com/stitchfix/flotilla-os/state"
	"io/ioutil"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	"strings"
	"time"
)

//
// EKSExecutionEngine submits runs to EKS.
//
type EKSExecutionEngine struct {
	kClient       *kubernetes.Clientset
	metricsClient *metricsv.Clientset
	adapter       adapter.EKSAdapter
	qm            queue.Manager
	log           flotillaLog.Logger
	jobQueue      string
	jobNamespace  string
	jobTtl        int
	jobSA         string
	schedulerName string
	ec2Client     *ec2.EC2
}

//
// Initialize configures the EKSExecutionEngine and initializes internal clients
//
func (ee *EKSExecutionEngine) Initialize(conf config.Config) error {
	kStr, err := base64.StdEncoding.DecodeString(conf.GetString("eks.kubeconfig"))
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(conf.GetString("eks.kubeconfig_path"), kStr, 0644)
	if err != nil {
		return err
	}

	kubeconfig := flag.String("kubeconfig",
		conf.GetString("eks.kubeconfig_path"),
		"(optional) absolute tmpPath to the kubeconfig file")

	flag.Parse()

	clientConf, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return err
	}

	kClient, err := kubernetes.NewForConfig(clientConf)
	if err != nil {
		return err
	}

	ee.jobQueue = conf.GetString("eks.job_queue")
	ee.schedulerName = "default-scheduler"

	if conf.IsSet("eks.scheduler_name") {
		ee.schedulerName = conf.GetString("eks.scheduler_name")
	}

	ee.jobNamespace = conf.GetString("eks.job_namespace")
	ee.jobTtl = conf.GetInt("eks.job_ttl")
	ee.kClient = kClient
	ee.jobSA = conf.GetString("eks.service_account")
	ee.metricsClient = metricsv.NewForConfigOrDie(clientConf)

	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(conf.GetString("aws_default_region"))}))

	ee.ec2Client = ec2.New(sess)

	adapt, err := adapter.NewEKSAdapter()

	if err != nil {
		return err
	}

	ee.adapter = adapt
	return nil
}

func (ee *EKSExecutionEngine) Execute(td state.Definition, run state.Run) (state.Run, bool, error) {
	job, err := ee.adapter.AdaptFlotillaDefinitionAndRunToJob(td, run, ee.jobSA, ee.schedulerName)

	result, err := ee.kClient.BatchV1().Jobs(ee.jobNamespace).Create(&job)
	if err != nil {
		// Job is already submitted, don't retry
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return run, false, nil
		}

		// Job spec is invalid, don't retry.
		if strings.Contains(strings.ToLower(err.Error()), "is invalid") {
			exitReason := err.Error()
			run.ExitReason = &exitReason
			return run, false, err
		}

		// Legitimate submit error, retryable.
		_ = metrics.Increment(metrics.EngineEKSExecute, []string{string(metrics.StatusFailure)}, 1)
		return run, true, err
	}

	_ = metrics.Increment(metrics.EngineEKSExecute, []string{string(metrics.StatusSuccess)}, 1)

	run, _ = ee.getPodName(run)
	adaptedRun, err := ee.adapter.AdaptJobToFlotillaRun(result, run, nil)

	return adaptedRun, false, nil
}

func (ee *EKSExecutionEngine) getPodName(run state.Run) (state.Run, error) {
	podList, err := ee.getPodList(run)

	if err != nil {
		return run, err
	}

	if podList != nil && podList.Items != nil && len(podList.Items) > 0 {
		pod := podList.Items[len(podList.Items)-1]
		run.PodName = &pod.Name
		run.Namespace = &pod.Namespace
		if pod.Spec.Containers != nil && len(pod.Spec.Containers) > 0 {
			container := pod.Spec.Containers[len(pod.Spec.Containers)-1]
			run.ContainerName = &container.Name
			cpu := container.Resources.Limits.Cpu().ScaledValue(resource.Milli)
			run.Cpu = &cpu
			run = ee.getInstanceDetails(pod, run)
			mem := container.Resources.Limits.Memory().ScaledValue(resource.Mega)
			run.Memory = &mem
			_ = ee.log.Log("job-name=", run.RunID, "pod-name=", run.PodName, "cpu", cpu, "mem", mem)
		}
	}
	return run, nil
}

func (ee *EKSExecutionEngine) getInstanceDetails(pod v1.Pod, run state.Run) (state.Run) {
	if len(pod.Spec.NodeName) > 0 {
		run.InstanceDNSName = pod.Spec.NodeName
	}
	return run
}

func (ee *EKSExecutionEngine) getPodList(run state.Run) (*v1.PodList, error) {
	podList, err := ee.kClient.CoreV1().Pods(ee.jobNamespace).List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", run.RunID),
	})
	return podList, err
}

func (ee *EKSExecutionEngine) Terminate(run state.Run) error {
	gracePeriod := int64(0)
	deletionPropagation := metav1.DeletePropagationBackground
	_ = ee.log.Log("terminating run=", run.RunID)
	deleteOptions := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &deletionPropagation,
	}
	err := ee.kClient.BatchV1().Jobs(ee.jobNamespace).Delete(run.RunID, deleteOptions)
	if run.PodName != nil {
		_ = ee.kClient.CoreV1().Pods(ee.jobNamespace).Delete(*run.PodName, deleteOptions)
	}

	if err != nil {
		_ = metrics.Increment(metrics.EngineEKSTerminate, []string{string(metrics.StatusFailure)}, 1)
		return err
	}

	_ = metrics.Increment(metrics.EngineEKSTerminate, []string{string(metrics.StatusSuccess)}, 1)
	return nil
}

func (ee *EKSExecutionEngine) Enqueue(run state.Run) error {
	// Get qurl
	qurl, err := ee.qm.QurlFor(ee.jobQueue, false)
	if err != nil {
		_ = metrics.Increment(metrics.EngineEKSEnqueue, []string{string(metrics.StatusFailure)}, 1)
		return errors.Wrapf(err, "problem getting queue url for [%s]", run.ClusterName)
	}

	// Queue run
	if err = ee.qm.Enqueue(qurl, run); err != nil {
		_ = metrics.Increment(metrics.EngineEKSEnqueue, []string{string(metrics.StatusFailure)}, 1)
		return errors.Wrapf(err, "problem enqueing run [%s] to queue [%s]", run.RunID, qurl)
	}

	_ = metrics.Increment(metrics.EngineEKSEnqueue, []string{string(metrics.StatusSuccess)}, 1)
	return nil
}

func (ee *EKSExecutionEngine) PollRuns() ([]RunReceipt, error) {
	qurl, err := ee.qm.QurlFor(ee.jobQueue, false)
	if err != nil {
		return nil, errors.Wrap(err, "problem listing queues to poll")
	}
	queues := []string{qurl}
	var runs []RunReceipt
	for _, qurl := range queues {
		//
		// Get new queued Run
		//
		runReceipt, err := ee.qm.ReceiveRun(qurl)

		if err != nil {
			return runs, errors.Wrapf(err, "problem receiving run from queue url [%s]", qurl)
		}

		if runReceipt.Run == nil {
			continue
		}

		runs = append(runs, RunReceipt{runReceipt})
	}
	return runs, nil
}

//
// PollStatus is a dummy function as EKS does not emit task status
// change events.
//
func (ee *EKSExecutionEngine) PollStatus() (RunReceipt, error) {
	//eventList, err:= ee.kClient.CoreV1().Events(ee.jobNamespace).List(metav1.ListOptions{
	//	LabelSelector:       "",
	//})
	//
	//if err != nil {
	//	return RunReceipt{}, errors.Wrapf(err, "problem receiving events from eks")
	//}
	return RunReceipt{}, nil
}

//
// Define returns a blank task definition and an error for the EKS engine.
//
func (ee *EKSExecutionEngine) Define(td state.Definition) (state.Definition, error) {
	return td, errors.New("Definition of tasks are only for ECSs.")
}

//
// Deregister returns an error for the EKS engine.
//
func (ee *EKSExecutionEngine) Deregister(definition state.Definition) error {
	return errors.Errorf("EKSExecutionEngine does not allow for deregistering of task definitions.")
}

func (ee *EKSExecutionEngine) Get(run state.Run) (state.Run, error) {
	job, err := ee.kClient.BatchV1().Jobs(ee.jobNamespace).Get(run.RunID, metav1.GetOptions{})

	if err != nil {
		return state.Run{}, errors.Errorf("error getting kubernetes job %s", err)
	}
	updates, err := ee.adapter.AdaptJobToFlotillaRun(job, run, nil)

	if err != nil {
		return state.Run{}, errors.Errorf("error adapting kubernetes job to flotilla run %s", err)
	}

	return updates, nil
}

func (ee *EKSExecutionEngine) GetEvents(run state.Run) (state.PodEventList, error) {
	if run.PodName == nil {
		return state.PodEventList{}, nil
	}
	eventList, err := ee.kClient.CoreV1().Events(ee.jobNamespace).List(metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.name==%s", *run.PodName)})
	if err != nil {
		return state.PodEventList{}, errors.Errorf("error getting kubernetes event for flotilla run %s", err)
	}

	_ = ee.log.Log("message", "getting events", "run_id", run.RunID, "events", len(eventList.Items))
	var podEvents []state.PodEvent
	for _, e := range eventList.Items {
		eTime := e.FirstTimestamp.Time
		runEvent := state.PodEvent{
			Message:      e.Message,
			Timestamp:    &eTime,
			EventType:    e.Type,
			Reason:       e.Reason,
			SourceObject: e.ObjectMeta.Name,
		}

		if strings.Contains(e.Reason, "TriggeredScaleUp") {
			source := fmt.Sprintf("source:%s", e.ObjectMeta.Name)
			_ = metrics.Increment(metrics.EngineEKSNodeTriggeredScaledUp, []string{source}, 1)
		}
		podEvents = append(podEvents, runEvent)
	}

	podEventList := state.PodEventList{
		Total:     len(podEvents),
		PodEvents: podEvents,
	}

	return podEventList, nil
}

func (ee *EKSExecutionEngine) FetchPodMetrics(run state.Run) (state.Run, error) {
	if run.PodName != nil {
		podMetrics, err := ee.metricsClient.MetricsV1beta1().PodMetricses(ee.jobNamespace).Get(*run.PodName, metav1.GetOptions{})
		if err != nil {
			return run, err
		}
		if len(podMetrics.Containers) > 0 {
			containerMetrics := podMetrics.Containers[0]
			mem := containerMetrics.Usage.Memory().ScaledValue(resource.Mega)
			if run.MaxMemoryUsed == nil || *run.MaxMemoryUsed == 0 || *run.MaxMemoryUsed < mem {
				run.MaxMemoryUsed = &mem
			}

			cpu := containerMetrics.Usage.Cpu().MilliValue()
			if run.MaxCpuUsed == nil || *run.MaxCpuUsed == 0 || *run.MaxCpuUsed < cpu {
				run.MaxCpuUsed = &cpu
			}
		}
		return run, nil
	}
	return run, errors.New("no pod associated with the run.")
}

func (ee *EKSExecutionEngine) FetchUpdateStatus(run state.Run) (state.Run, error) {
	job, err := ee.kClient.BatchV1().Jobs(ee.jobNamespace).Get(run.RunID, metav1.GetOptions{})

	if err != nil {
		return run, err
	}

	var mostRecentPod *v1.Pod
	var mostRecentPodCreationTimestamp metav1.Time

	podList, err := ee.getPodList(run)

	if err == nil && podList != nil && podList.Items != nil && len(podList.Items) > 0 {
		_ = ee.log.Log("message", "iterating over pods", "podList length", len(podList.Items))

		// Iterate over associated pods to find the most recent.
		for _, p := range podList.Items {
			if mostRecentPodCreationTimestamp.Before(&p.CreationTimestamp) || len(podList.Items) == 1 {
				mostRecentPod = &p
				mostRecentPodCreationTimestamp = p.CreationTimestamp
			}
		}

		// If the run doesn't have an associated pod name yet OR
		// there is a newer pod (i.e. the old pod was killed),
		// update it.
		if mostRecentPod != nil && (run.PodName == nil || mostRecentPod.Name != *run.PodName) {

			_ = ee.log.Log("message", "found new pod for run", "prev_pod_name", run.PodName, "next_pod_name", mostRecentPod.Name)

			if run.PodName != nil && mostRecentPod.Name != *run.PodName {
				_ = metrics.Increment(metrics.EngineEKSRunPodnameChange, []string{}, 1)
			}

			run.PodName = &mostRecentPod.Name
			run = ee.getInstanceDetails(*mostRecentPod, run)
		}

		// Pod didn't change, but Instance information is not populated.
		if mostRecentPod != nil && len(run.InstanceDNSName) == 0 {
			run = ee.getInstanceDetails(*mostRecentPod, run)
		}

		if mostRecentPod != nil && mostRecentPod.Spec.Containers != nil && len(mostRecentPod.Spec.Containers) > 0 {
			container := mostRecentPod.Spec.Containers[len(mostRecentPod.Spec.Containers)-1]
			run.ContainerName = &container.Name
			cpu := container.Resources.Limits.Cpu().ScaledValue(resource.Milli)
			run.Cpu = &cpu
			mem := container.Resources.Limits.Memory().ScaledValue(resource.Mega)
			run.Memory = &mem

		}
	}

	run, _ = ee.FetchPodMetrics(run)
	hoursBack := time.Now().Add(-24 * time.Hour)

	events, err := ee.GetEvents(run)

	if err == nil && len(events.PodEvents) > 0 {
		newEvents := events.PodEvents
		if run.PodEvents != nil && len(*run.PodEvents) > 0 {
			priorEvents := *run.PodEvents
			for _, newEvent := range newEvents {
				unseen := true
				for _, priorEvent := range priorEvents {
					if priorEvent.Equal(newEvent) {
						unseen = false
						break
					}
				}
				if unseen {
					priorEvents = append(priorEvents, newEvent)
				}
			}
			run.PodEvents = &priorEvents
		} else {
			run.PodEvents = &newEvents
		}
	}

	// Handle edge case for dangling jobs.
	// Run used to have a pod and now it is not there, job is older than 24 hours. Terminate it.
	if err == nil && podList != nil && podList.Items != nil && len(podList.Items) == 0 && run.PodName != nil && run.QueuedAt.Before(hoursBack) {
		err = ee.Terminate(run)
		if err == nil {
			job.Status.Failed = 1
			mostRecentPod = nil
		}
	}

	return ee.adapter.AdaptJobToFlotillaRun(job, run, mostRecentPod)
}
