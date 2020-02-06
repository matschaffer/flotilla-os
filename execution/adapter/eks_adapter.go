package adapter

import (
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/stitchfix/flotilla-os/state"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"time"
)

type EKSAdapter interface {
	AdaptJobToFlotillaRun(job *batchv1.Job, run state.Run, pod *corev1.Pod) (state.Run, error)
	AdaptFlotillaDefinitionAndRunToJob(executable state.Executable, run state.Run, sa string, schedulerName string, manager state.Manager, araEnabled bool) (batchv1.Job, error)
}
type eksAdapter struct{}

//
// NewEKSAdapter configures and returns an eks adapter for translating
// from EKS api specific objects to our representation
//
func NewEKSAdapter() (EKSAdapter, error) {
	adapter := eksAdapter{}
	return &adapter, nil
}

func (a *eksAdapter) AdaptJobToFlotillaRun(job *batchv1.Job, run state.Run, pod *corev1.Pod) (state.Run, error) {
	updated := run
	if job.Status.Active == 1 && job.Status.CompletionTime == nil {
		updated.Status = state.StatusRunning
	} else if job.Status.Succeeded == 1 {
		var exitCode int64 = 0
		updated.Status = state.StatusStopped
		updated.ExitCode = &exitCode
	} else if job.Status.Failed == 1 {
		var exitCode int64 = 1
		updated.Status = state.StatusStopped
		if pod != nil {
			if pod.Status.ContainerStatuses != nil && len(pod.Status.ContainerStatuses) > 0 {
				containerStatus := pod.Status.ContainerStatuses[len(pod.Status.ContainerStatuses)-1]
				if containerStatus.State.Terminated != nil {
					updated.ExitReason = &containerStatus.State.Terminated.Reason
					exitCode = int64(containerStatus.State.Terminated.ExitCode)
				}
			}
		}
		updated.ExitCode = &exitCode
	}

	if pod != nil && len(pod.Spec.Containers) > 0 {
		container := pod.Spec.Containers[0]
		//First three lines are injected by Flotilla, strip those out.
		if len(container.Command) > 3 {
			cmd := strings.Join(container.Command[3:], "\n")
			updated.Command = &cmd
		}
	}

	if job != nil && job.Status.StartTime != nil {
		updated.StartedAt = &job.Status.StartTime.Time
	}

	if updated.Status == state.StatusStopped {
		if job != nil && job.Status.CompletionTime != nil {
			updated.FinishedAt = &job.Status.CompletionTime.Time
		} else {
			finishedAt := time.Now()
			updated.FinishedAt = &finishedAt
		}
	}
	return updated, nil
}

func (a *eksAdapter) AdaptFlotillaDefinitionAndRunToJob(executable state.Executable, run state.Run, sa string, schedulerName string, manager state.Manager, araEnabled bool) (batchv1.Job, error) {
	cmd := ""

	if run.Command != nil && len(*run.Command) > 0 {
		cmd = *run.Command
	}

	cmdSlice := a.constructCmdSlice(cmd)
	cmd = strings.Join(cmdSlice[3:], "\n")
	run.Command = &cmd
	resourceRequirements, run := a.constructResourceRequirements(executable, run, manager, araEnabled)

	container := corev1.Container{
		Name:      run.RunID,
		Image:     run.Image,
		Command:   cmdSlice,
		Resources: resourceRequirements,
		Env:       a.envOverrides(executable, run),
		Ports:     a.constructContainerPorts(executable),
	}

	affinity := a.constructAffinity(executable, run)
	annotations := map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"}

	activeDeadlineSeconds := state.SpotActiveDeadlineSeconds
	if *run.NodeLifecycle == state.OndemandLifecycle {
		activeDeadlineSeconds = state.OndemandActiveDeadlineSeconds
	}

	jobSpec := batchv1.JobSpec{
		TTLSecondsAfterFinished: &state.TTLSecondsAfterFinished,
		ActiveDeadlineSeconds:   &activeDeadlineSeconds,
		BackoffLimit:            &state.EKSBackoffLimit,

		Template: corev1.PodTemplateSpec{
			ObjectMeta: v1.ObjectMeta{
				Annotations: annotations,
			},
			Spec: corev1.PodSpec{
				SchedulerName:      schedulerName,
				Containers:         []corev1.Container{container},
				RestartPolicy:      corev1.RestartPolicyNever,
				ServiceAccountName: sa,
				Affinity:           affinity,
			},
		},
	}

	eksJob := batchv1.Job{
		Spec: jobSpec,
		ObjectMeta: v1.ObjectMeta{
			Name: run.RunID,
		},
	}

	return eksJob, nil
}

func (a *eksAdapter) constructContainerPorts(executable state.Executable) []corev1.ContainerPort {
	var containerPorts []corev1.ContainerPort
	executableResources := executable.GetExecutableResources()
	if executableResources.Ports != nil && len(*executableResources.Ports) > 0 {
		for _, port := range *executableResources.Ports {
			containerPorts = append(containerPorts, corev1.ContainerPort{
				ContainerPort: int32(port),
			})
		}
	}
	return containerPorts
}

func (a *eksAdapter) constructAffinity(executable state.Executable, run state.Run) *corev1.Affinity {
	affinity := &corev1.Affinity{}
	executableResources := executable.GetExecutableResources()
	var requiredMatch []corev1.NodeSelectorRequirement

	gpuNodeTypes := []string{"p3.2xlarge", "p3.8xlarge", "p3.16xlarge"}
	cpuNodeTypes := []string{"c5.2xlarge", "c5.4xlarge", "c5.9xlarge"}

	var nodeLifecycle []string
	if *run.NodeLifecycle == state.OndemandLifecycle {
		nodeLifecycle = append(nodeLifecycle, "normal")
	} else {
		nodeLifecycle = append(nodeLifecycle, "spot")
	}

	if executableResources.Gpu == nil || *executableResources.Gpu <= 0 {
		requiredMatch = append(requiredMatch, corev1.NodeSelectorRequirement{
			Key:      "beta.kubernetes.io/instance-type",
			Operator: corev1.NodeSelectorOpNotIn,
			Values:   gpuNodeTypes,
		})

		//For high cpu jobs - assign to c5 node types.
		if run.Memory != nil &&
			run.Cpu != nil &&
			*run.Cpu > int64(0) &&
			*run.Memory > int64(0) &&
			float64(*run.Cpu)/float64(*run.Memory) >= 0.5 {
			requiredMatch = append(requiredMatch, corev1.NodeSelectorRequirement{
				Key:      "beta.kubernetes.io/instance-type",
				Operator: corev1.NodeSelectorOpIn,
				Values:   cpuNodeTypes,
			})
		}
	}

	requiredMatch = append(requiredMatch, corev1.NodeSelectorRequirement{
		Key:      "kubernetes.io/lifecycle",
		Operator: corev1.NodeSelectorOpIn,
		Values:   nodeLifecycle,
	})

	affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: requiredMatch,
					},
				},
			},
		},
	}

	return affinity
}

func (a *eksAdapter) constructResourceRequirements(executable state.Executable, run state.Run, manager state.Manager, araEnabled bool) (corev1.ResourceRequirements, state.Run) {
	limits := make(corev1.ResourceList)
	requests := make(corev1.ResourceList)
	cpuLimit, memLimit, cpuRequest, memRequest := a.adaptiveResources(executable, run, manager, araEnabled)

	cpuLimitQuantity := resource.MustParse(fmt.Sprintf("%dm", cpuLimit))
	cpuRequestQuantity := resource.MustParse(fmt.Sprintf("%dm", cpuRequest))

	memLimitQuantity := resource.MustParse(fmt.Sprintf("%dM", memLimit))
	memRequestQuantity := resource.MustParse(fmt.Sprintf("%dM", memRequest))

	limits[corev1.ResourceCPU] = cpuLimitQuantity
	limits[corev1.ResourceMemory] = memLimitQuantity

	requests[corev1.ResourceCPU] = cpuRequestQuantity
	requests[corev1.ResourceMemory] = memRequestQuantity

	executableResources := executable.GetExecutableResources()
	if executableResources.Gpu != nil && *executableResources.Gpu > 0 {
		limits["nvidia.com/gpu"] = resource.MustParse(fmt.Sprintf("%d", *executableResources.Gpu))
		requests["nvidia.com/gpu"] = resource.MustParse(fmt.Sprintf("%d", *executableResources.Gpu))
		run.NodeLifecycle = &state.OndemandLifecycle
	}

	run.Memory = aws.Int64(memRequestQuantity.ScaledValue(resource.Mega))
	run.Cpu = aws.Int64(cpuRequestQuantity.ScaledValue(resource.Milli))

	resourceRequirements := corev1.ResourceRequirements{
		Limits:   limits,
		Requests: requests,
	}
	return resourceRequirements, run
}

func (a *eksAdapter) adaptiveResources(executable state.Executable, run state.Run, manager state.Manager, araEnabled bool) (int64, int64, int64, int64) {
	cpuLimit, memLimit := a.getResourceDefaults(run, executable)
	cpuRequest, memRequest := a.getResourceDefaults(run, executable)
	executableResources := executable.GetExecutableResources()
	if araEnabled && executableResources.AdaptiveResourceAllocation != nil && *executableResources.AdaptiveResourceAllocation == true {
		// Check if last run was a OOM, in that case only increase memory
		lastRun := a.getLastRun(manager, run)
		if lastRun.ExitReason != nil && strings.Contains(*lastRun.ExitReason, "OOMKilled") {
			memRequest = int64(float64(*lastRun.Memory) * 1.75)
			cpuRequest = *lastRun.Cpu
		} else {
			// If last run wasn't an OOM, estimate based on successful runs.
			estimatedResources, err := manager.EstimateRunResources(*executable.GetExecutableID(), run.RunID)
			if err == nil {
				cpuRequest = estimatedResources.Cpu
				memRequest = estimatedResources.Memory

			}
		}
	}
	if cpuRequest > cpuLimit {
		cpuLimit = cpuRequest
	}

	if memRequest > memLimit {
		memLimit = memRequest
	}

	cpuRequest, memRequest = a.checkResourceBounds(cpuRequest, memRequest)
	cpuLimit, memLimit = a.checkResourceBounds(cpuLimit, memLimit)

	return cpuLimit, memLimit, cpuRequest, memRequest
}

func (a *eksAdapter) checkResourceBounds(cpu int64, mem int64) (int64, int64) {
	if cpu < state.MinCPU {
		cpu = state.MinCPU
	}
	if cpu > state.MaxCPU {
		cpu = state.MaxCPU
	}
	if mem < state.MinMem {
		mem = state.MinMem
	}
	if mem > state.MaxMem {
		mem = state.MaxMem
	}
	return cpu, mem
}

func (a *eksAdapter) getResourceDefaults(run state.Run, executable state.Executable) (int64, int64) {
	// 1. Init with the global defaults
	cpu := state.MinCPU
	mem := state.MinMem
	executableResources := executable.GetExecutableResources()

	// 2. Look up Run level
	// 3. If not at Run level check Definitions
	if run.Cpu != nil && *run.Cpu != 0 {
		cpu = *run.Cpu
	} else {
		if executableResources.Cpu != nil && *executableResources.Cpu != 0 {
			cpu = *executableResources.Cpu
		}
	}
	if run.Memory != nil && *run.Memory != 0 {
		mem = *run.Memory
	} else {
		if executableResources.Memory != nil && *executableResources.Memory != 0 {
			mem = *executableResources.Memory
		}
	}
	// 4. Override for very large memory requests.
	// Remove after migration.
	if mem >= 36864 && mem < 131072 && (executableResources.Gpu == nil || *executableResources.Gpu == 0) {
		// using the 8x ratios between cpu and memory ~ r5 class of instances
		cpuOverride := mem / 8
		if cpuOverride > cpu {
			cpu = cpuOverride
		}
	}

	return cpu, mem
}

func (a *eksAdapter) getLastRun(manager state.Manager, run state.Run) state.Run {
	var lastRun state.Run
	runList, err := manager.ListRuns(1, 0, "started_at", "desc", map[string][]string{
		"queued_at_since": {
			time.Now().AddDate(0, 0, -7).Format(time.RFC3339),
		},
		"status":        {state.StatusStopped},
		"command":       {strings.Replace(*run.Command, "'", "''", -1)},
		"definition_id": {run.DefinitionID},
	}, nil, []string{state.EKSEngine})
	if err == nil && len(runList.Runs) > 0 {
		lastRun = runList.Runs[0]
	}
	return lastRun
}

func (a *eksAdapter) constructCmdSlice(cmdString string) []string {
	bashCmd := "bash"
	optLogin := "-l"
	optStr := "-cex"
	return []string{bashCmd, optLogin, optStr, cmdString}
}

func (a *eksAdapter) envOverrides(executable state.Executable, run state.Run) []corev1.EnvVar {
	pairs := make(map[string]string)
	resources := executable.GetExecutableResources()
	for _, ev := range *resources.Env {
		name := a.sanitizeEnvVar(ev.Name)
		value := ev.Value
		pairs[name] = value
	}

	for _, ev := range *run.Env {
		name := a.sanitizeEnvVar(ev.Name)
		value := ev.Value
		pairs[name] = value
	}

	var res []corev1.EnvVar
	for key := range pairs {
		if len(key) > 0 {
			res = append(res, corev1.EnvVar{
				Name:  key,
				Value: pairs[key],
			})
		}
	}
	return res
}

func (a *eksAdapter) sanitizeEnvVar(key string) string {
	// Environment variable can't start with a $
	if strings.HasPrefix(key, "$") {
		key = strings.Replace(key, "$", "", 1)
	}
	// Environment variable names can't contain spaces.
	key = strings.Replace(key, " ", "", -1)
	return key
}
