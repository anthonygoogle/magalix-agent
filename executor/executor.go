package executor

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/MagalixCorp/magalix-agent/v2/client"
	"github.com/MagalixCorp/magalix-agent/v2/kuber"
	"github.com/MagalixCorp/magalix-agent/v2/proto"
	"github.com/MagalixCorp/magalix-agent/v2/utils"
	"github.com/MagalixTechnologies/core/logger"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kv1 "k8s.io/api/core/v1"
)

const (
	automationsBufferLength  = 1000
	automationsBufferTimeout = 10 * time.Second

	automationsFeedbackExpiryTime     = 30 * time.Minute
	automationsFeedbackExpiryCount    = 0
	automationsFeedbackExpiryPriority = 10
	automationsFeedbackExpiryRetries  = 5
	automationsExecutionTimeout       = 15 * time.Minute
	podStatusSleep                    = 15 * time.Second
)

type EntitiesFinder interface {
	FindController(namespaceName string, controllerKind string, controllerName string) (*unstructured.Unstructured, error)
	FindContainer(namespaceName string, controllerKind string, controllerName string, containerName string) (*kv1.Container, error)
}

// Executor Automation executor
type Executor struct {
	client          *client.Client
	kube            *kuber.Kube
	entitiesFinder  EntitiesFinder
	dryRun          bool
	workersCount    int
	automationsChan chan *proto.PacketAutomation
	inProgressJobs  map[string]bool
}

type Replica struct {
	name     string
	replicas int32
	time     time.Time
}

// InitExecutor creates a new executor then starts it
func InitExecutor(
	client *client.Client,
	kube *kuber.Kube,
	entitiesFinder EntitiesFinder,
	workersCount int,
	dryRun bool,
) *Executor {
	e := NewExecutor(client, kube, entitiesFinder, workersCount, dryRun)
	e.startWorkers()
	return e
}

// NewExecutor creates a new executor
func NewExecutor(
	client *client.Client,
	kube *kuber.Kube,
	entitiesFinder EntitiesFinder,
	workersCount int,
	dryRun bool,
) *Executor {
	executor := &Executor{
		client:         client,
		kube:           kube,
		entitiesFinder: entitiesFinder,
		dryRun:         dryRun,

		workersCount:    workersCount,
		inProgressJobs:  map[string]bool{},
		automationsChan: make(chan *proto.PacketAutomation, automationsBufferLength),
	}

	return executor
}

func (executor *Executor) backoff(
	fn func() error, sleep time.Duration, maxRetries int,
) error {
	return utils.WithBackoff(
		fn,
		utils.Backoff{
			Sleep:      sleep,
			MaxRetries: maxRetries,
		},
	)
}

func (executor *Executor) startWorkers() {
	// this method should be called one time only
	for i := 0; i < executor.workersCount; i++ {
		go executor.executorWorker()
	}
}

func (executor *Executor) handleExecutionError(
	automation *proto.PacketAutomation, err error,
) *proto.PacketAutomationFeedbackRequest {
	logger.Errorw("unable to execute automation", "error", err, "automation-id", automation.ID)

	return makeAutomationFailedResponse(automation, err.Error())
}
func (executor *Executor) handleExecutionSkipping(
	aut *proto.PacketAutomation, msg string,
) *proto.PacketAutomationFeedbackRequest {
	logger.Infof("skipping automation execution", "msg", msg, "automation-id", aut.ID)

	return makeAutomationFailedResponse(aut, msg)
}

func (executor *Executor) Listener(in []byte) (out []byte, err error) {
	var automation proto.PacketAutomation
	if err = proto.DecodeSnappy(in, &automation); err != nil {
		return
	}
	_, found := executor.inProgressJobs[automation.ID]
	if !found {
		executor.inProgressJobs[automation.ID] = true
		convertAutomationMemoryFromKiloByteToMegabyte(&automation)

		err = executor.submitAutomation(&automation, automationsBufferTimeout)
		if err != nil {
			errMessage := err.Error()
			return proto.EncodeSnappy(proto.PacketAutomationResponse{
				ID:    automation.ID,
				Error: &errMessage,
			})
		}
	}

	return proto.EncodeSnappy(proto.PacketAutomationResponse{})
}

func convertAutomationMemoryFromKiloByteToMegabyte(automation *proto.PacketAutomation) {
	if automation.ContainerResources.Requests != nil && automation.ContainerResources.Requests.Memory != nil {
		*automation.ContainerResources.Requests.Memory = *automation.ContainerResources.Requests.Memory / 1024
	}
	if automation.ContainerResources.Limits != nil && automation.ContainerResources.Limits.Memory != nil {
		*automation.ContainerResources.Limits.Memory = *automation.ContainerResources.Limits.Memory / 1024
	}
}

func (executor *Executor) submitAutomation(
	automation *proto.PacketAutomation,
	timeout time.Duration,
) error {
	select {
	case executor.automationsChan <- automation:
	case <-time.After(timeout):
		return fmt.Errorf(
			"timeout (after %s) waiting to push automation into buffer chan",
			automationsBufferTimeout,
		)
	}
	return nil
}

func (executor *Executor) executorWorker() {
	for automation := range executor.automationsChan {
		// TODO: execute automations in batches
		response, err := executor.execute(automation)
		if err != nil {
			logger.Errorw(
				"unable to execute automation",
				"error", err,
				"automation-id", automation.ID,
			)
		}

		delete(executor.inProgressJobs, automation.ID)

		executor.client.Pipe(
			client.Package{
				Kind:        proto.PacketKindAutomationFeedback,
				ExpiryTime:  utils.After(automationsFeedbackExpiryTime),
				ExpiryCount: automationsFeedbackExpiryCount,
				Priority:    automationsFeedbackExpiryPriority,
				Retries:     automationsFeedbackExpiryRetries,
				Data:        response,
			},
		)
	}
}

func (executor *Executor) execute(
	automation *proto.PacketAutomation,
) (*proto.PacketAutomationFeedbackRequest, error) {
	c, err := executor.entitiesFinder.FindContainer(
		automation.NamespaceName,
		automation.ControllerKind,
		automation.ControllerName,
		automation.ContainerName,
	)

	if err != nil {
		return makeAutomationFailedResponse(automation, fmt.Sprintf("unable to get container details %s", err.Error())),
			fmt.Errorf("unable to get container details, error: %w", err)
	}

	var container kv1.Container
	err = utils.Transcode(c, &container)
	lg := logger.With(
		"automation-id", automation.ID,
		"namespace-name", automation.NamespaceName,
		"controller-name", automation.ControllerName,
		"controller-kind", automation.ControllerKind,
		"container-name", automation.ContainerName,
	)

	if err != nil {
		return makeAutomationFailedResponse(automation, fmt.Sprintf("unable to get container details %s", err.Error())),
			fmt.Errorf("unable to get container details, error: %w", err)
	}

	originalResources := buildOriginalResourcesFromContainer(container)
	recommendedResources := buildRecommendedResourcesFromAutomation(originalResources, automation)

	trace, _ := json.Marshal(recommendedResources)
	lg.Debugw(
		"executing automation",
		"dry run", executor.dryRun,
		"cpu unit", "milliCore",
		"memory unit", "mibiByte",
		"trace", string(trace),
	)

	if executor.dryRun {
		response := executor.handleExecutionSkipping(automation, "dry run enabled")
		return response, nil
	} else {
		skipped, err := executor.kube.SetResources(
			automation.ControllerKind,
			automation.ControllerName,
			automation.NamespaceName,
			recommendedResources,
		)
		if err != nil {
			// TODO: do we need to retry execution before fail?
			var response *proto.PacketAutomationFeedbackRequest
			if skipped {
				response = executor.handleExecutionSkipping(automation, err.Error())
			} else {
				response = executor.handleExecutionError(automation, err)
			}
			return response, nil
		}

		// short pooling to trigger pod status with max 15 minutes
		statusMap := make(map[kv1.PodPhase]string)
		statusMap[kv1.PodRunning] = "pods restarted successfully"
		statusMap[kv1.PodFailed] = "pods failed to restart"
		statusMap[kv1.PodUnknown] = "pods status is unknown"

		result, msg, targetPodCount, runningPods := executor.podsStatusHandler(
			automation.ControllerName,
			automation.NamespaceName,
			automation.ControllerKind,
			statusMap,
		)

		//rollback in case of failed to restart all pods
		if runningPods < targetPodCount {

			msg = statusMap[kv1.PodFailed]
			result = proto.AutomationFailed

			// execute the automation with old values to rollback
			_, err := executor.kube.SetResources(
				automation.ControllerKind,
				automation.ControllerName,
				automation.NamespaceName,
				originalResources,
			)

			if err != nil {
				lg.Warn("can't rollback automation")
			}
		}

		lg.Debug(msg)

		return &proto.PacketAutomationFeedbackRequest{
			ID:             automation.ID,
			NamespaceName:  automation.NamespaceName,
			ControllerName: automation.ControllerName,
			ControllerKind: automation.ControllerKind,
			ContainerName:  automation.ContainerName,
			Status:         result,
			Message:        msg,
		}, nil
	}
}

func makeAutomationFailedResponse(automation *proto.PacketAutomation, msg string) *proto.PacketAutomationFeedbackRequest {
	return &proto.PacketAutomationFeedbackRequest{
		ID:             automation.ID,
		NamespaceName:  automation.NamespaceName,
		ControllerKind: automation.ControllerName,
		ControllerName: automation.ControllerName,
		ContainerName:  automation.ContainerName,
		Status:         proto.AutomationFailed,
		Message:        msg,
	}
}

func buildRecommendedResourcesFromAutomation(originalResources kuber.TotalResources, automation *proto.PacketAutomation) kuber.TotalResources {
	recommendedResources := kuber.TotalResources{
		Containers: []kuber.ContainerResourcesRequirements{
			{
				Name:     originalResources.Containers[0].Name,
				Requests: new(kuber.RequestLimit),
				Limits:   new(kuber.RequestLimit),
			},
		},
	}

	// Copy original values
	if originalResources.Containers[0].Requests.CPU != nil {
		_cpuRequest := *originalResources.Containers[0].Requests.CPU
		recommendedResources.Containers[0].Requests.CPU = &_cpuRequest
	}
	if originalResources.Containers[0].Requests.Memory != nil {
		_memoryRequest := *originalResources.Containers[0].Requests.Memory
		recommendedResources.Containers[0].Requests.Memory = &_memoryRequest
	}
	if originalResources.Containers[0].Limits.CPU != nil {
		_cpuLimit := *originalResources.Containers[0].Limits.CPU
		recommendedResources.Containers[0].Limits.CPU = &_cpuLimit
	}
	if originalResources.Containers[0].Limits.Memory != nil {
		_memoryLimit := *originalResources.Containers[0].Limits.Memory
		recommendedResources.Containers[0].Limits.Memory = &_memoryLimit
	}

	// Override with values from automation
	if automation.ContainerResources.Requests != nil {
		if automation.ContainerResources.Requests.CPU != nil {
			recommendedResources.Containers[0].Requests.CPU = automation.ContainerResources.Requests.CPU
		}
		if automation.ContainerResources.Requests.Memory != nil {
			recommendedResources.Containers[0].Requests.Memory = automation.ContainerResources.Requests.Memory
		}
	}
	if automation.ContainerResources.Limits != nil {
		if automation.ContainerResources.Limits.CPU != nil {
			recommendedResources.Containers[0].Limits.CPU = automation.ContainerResources.Limits.CPU
		}
		if automation.ContainerResources.Limits.Memory != nil {
			recommendedResources.Containers[0].Limits.Memory = automation.ContainerResources.Limits.Memory
		}
	}

	return recommendedResources
}

func buildOriginalResourcesFromContainer(container kv1.Container) kuber.TotalResources {
	memoryLimit := container.Resources.Limits.Memory().Value()
	memoryRequest := container.Resources.Requests.Memory().Value()
	cpuLimit := container.Resources.Limits.Cpu().MilliValue()
	cpuRequest := container.Resources.Requests.Cpu().MilliValue()

	originalResources := kuber.TotalResources{
		Containers: []kuber.ContainerResourcesRequirements{
			{
				Name:     container.Name,
				Requests: new(kuber.RequestLimit),
				Limits:   new(kuber.RequestLimit),
			},
		},
	}

	if memoryLimit != 0 {
		_memoryLimit := memoryLimit / 1024 / 1024
		originalResources.Containers[0].Limits.Memory = &_memoryLimit
	}
	if memoryRequest != 0 {
		_memoryRequest := memoryRequest / 1024 / 1024
		originalResources.Containers[0].Requests.Memory = &_memoryRequest
	}
	if cpuLimit != 0 {
		originalResources.Containers[0].Limits.CPU = &cpuLimit
	}
	if cpuRequest != 0 {
		originalResources.Containers[0].Requests.CPU = &cpuRequest
	}

	return originalResources
}
