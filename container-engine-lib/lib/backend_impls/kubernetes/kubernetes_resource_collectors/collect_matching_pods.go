package kubernetes_resource_collectors

import (
	"context"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/kubernetes/kubernetes_manager"
	"github.com/kurtosis-tech/stacktrace"
	apiv1 "k8s.io/api/core/v1"
)

// NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE
// Due to not having Go 1.18 generics yet, we have to do all this boilerplate in order to do generic filtering
//  on Kubernetes resources
// This entire file is intended to be copy-pasted if we need to create new CollectMatchingXXXXXX functions
// NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE NOTE

// TODO Remove all this when we have Go 1.18 generics
type podKubernetesResource struct {
	underlying apiv1.Pod
}
func (resource podKubernetesResource) getName() string {
	return resource.underlying.Name
}
func (resource podKubernetesResource) getLabels() map[string]string {
	return resource.underlying.Labels
}
func (resource podKubernetesResource) getUnderlying() interface{} {
	return resource.underlying
}

// TODO Remove all this when we have Go 1.18 generics
// NOTE: This function is intended to be copy-pasted to create new ones
func CollectMatchingPods(
	ctx context.Context,
	kubernetesManager *kubernetes_manager.KubernetesManager,
	namespace string,
	searchLabels map[string]string,
	postFilterLabelKey string,
	postFilterLabelValues map[string]bool,
) (
	map[string][]apiv1.Pod,
	error,
) {
	allObjects, err := kubernetesManager.GetPodsByLabels(ctx, namespace, searchLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting Kubernetes resources matching labels: %+v", searchLabels)
	}
	allKubernetesResources := []kubernetesResource{}
	for _, object := range allObjects.Items {
		allKubernetesResources = append(
			allKubernetesResources,
			podKubernetesResource{underlying: object},
		)
	}
	filteredKubernetesResources, err := postfilterKubernetesResources(allKubernetesResources, postFilterLabelKey, postFilterLabelValues)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred during postfiltering")
	}
	result := map[string][]apiv1.Pod{}
	for labelValue, matchingResources := range filteredKubernetesResources {
		castedObjects := []apiv1.Pod{}
		for _, resource := range matchingResources {
			casted, ok := resource.getUnderlying().(apiv1.Pod)
			if !ok {
				return nil, stacktrace.NewError("An error occurred downcasting Kubernetes resource object '%+v'", resource.getUnderlying())
			}
			castedObjects = append(castedObjects, casted)
		}
		result[labelValue] = castedObjects
	}
	return result, nil
}
