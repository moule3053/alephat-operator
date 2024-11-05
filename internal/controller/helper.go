package controller

import (
	"context"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	alephatv1 "github.com/moule3053/multiclusterresource/api/v1"
)

func getTargetClusters(multiClusterResource *alephatv1.MultiClusterResource, secretList *corev1.SecretList) ([]string, []string) {
	var targetClusters []string
	var missingClusters []string
	if multiClusterResource.Spec.TargetClusters != "" {
		targetClusters = strings.Split(multiClusterResource.Spec.TargetClusters, ",")
		for i := range targetClusters {
			targetClusters[i] = strings.TrimSpace(targetClusters[i])
		}

		// Check if all specified clusters have corresponding secrets
		for _, cluster := range targetClusters {
			found := false
			for _, secret := range secretList.Items {
				if strings.TrimPrefix(secret.Name, "kubeconfig-") == cluster {
					found = true
					break
				}
			}
			if !found {
				missingClusters = append(missingClusters, cluster)
			}
		}
	} else {
		for _, secret := range secretList.Items {
			if strings.HasPrefix(secret.Name, "kubeconfig-") {
				clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
				targetClusters = append(targetClusters, clusterName)
			}
		}
	}
	return targetClusters, missingClusters
}

func applyResourcesToCluster(ctx context.Context, logger logr.Logger, secret corev1.Secret, multiClusterResource *alephatv1.MultiClusterResource, r *MultiClusterResourceReconciler) error {
	clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
	kubeconfig := secret.Data["kubeconfig"]
	// Build the client for the target cluster
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		logger.Error(err, "Failed to build REST config for cluster", "cluster", clusterName)
		meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
			Type:    "Error",
			Status:  metav1.ConditionTrue,
			Reason:  "FailedToBuildRESTConfig",
			Message: "Failed to build REST config for cluster: " + clusterName + ". Error: " + err.Error(),
		})
		if statusErr := r.updateStatusWithRetry(ctx, multiClusterResource); statusErr != nil {
			logger.Error(statusErr, "Failed to update MultiClusterResource status")
		}
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "Failed to create dynamic client for cluster", "cluster", clusterName)
		meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
			Type:    "Error",
			Status:  metav1.ConditionTrue,
			Reason:  "FailedToCreateDynamicClient",
			Message: "Failed to create dynamic client for cluster: " + clusterName + ". Error: " + err.Error(),
		})
		if statusErr := r.updateStatusWithRetry(ctx, multiClusterResource); statusErr != nil {
			logger.Error(statusErr, "Failed to update MultiClusterResource status")
		}
		return err
	}
	// Apply the resources from the MultiClusterResource to the target cluster
	for _, manifest := range multiClusterResource.Spec.ResourceManifest {
		// Parse the YAML manifest into an unstructured.Unstructured object
		resource := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(manifest), resource); err != nil {
			logger.Error(err, "Failed to unmarshal YAML into unstructured", "manifest", manifest)
			meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
				Type:    "Error",
				Status:  metav1.ConditionTrue,
				Reason:  "FailedToUnmarshalYAML",
				Message: "Failed to unmarshal YAML into unstructured: " + manifest + ". Error: " + err.Error(),
			})
			if statusErr := r.updateStatusWithRetry(ctx, multiClusterResource); statusErr != nil {
				logger.Error(statusErr, "Failed to update MultiClusterResource status")
			}
			return err
		}
		// Determine if the resource is namespaced or cluster-scoped
		gvr, _ := meta.UnsafeGuessKindToResource(resource.GroupVersionKind())
		if isClusterScopedResource(resource) {
			_, err = dynamicClient.Resource(gvr).Create(ctx, resource, metav1.CreateOptions{})
		} else {
			if resource.GetNamespace() == "" {
				resource.SetNamespace("default")
			}
			_, err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Create(ctx, resource, metav1.CreateOptions{})
		}
		if err != nil {
			if errors.IsAlreadyExists(err) {
				// Fetch the existing resource to get the resourceVersion
				var existingResource *unstructured.Unstructured
				if isClusterScopedResource(resource) {
					existingResource, err = dynamicClient.Resource(gvr).Get(ctx, resource.GetName(), metav1.GetOptions{})
				} else {
					existingResource, err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Get(ctx, resource.GetName(), metav1.GetOptions{})
				}
				if err != nil {
					logger.Error(err, "Failed to get existing resource", "resource", resource.GetName(), "cluster", clusterName)
					return err
				}
				resource.SetResourceVersion(existingResource.GetResourceVersion())
				if isClusterScopedResource(resource) {
					_, err = dynamicClient.Resource(gvr).Update(ctx, resource, metav1.UpdateOptions{})
				} else {
					_, err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Update(ctx, resource, metav1.UpdateOptions{})
				}
				if err != nil {
					logger.Error(err, "Failed to update resource", "resource", resource.GetName(), "cluster", clusterName)
					meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
						Type:    "Error",
						Status:  metav1.ConditionTrue,
						Reason:  "FailedToUpdateResource",
						Message: "Failed to update resource: " + resource.GetName() + " in cluster: " + clusterName + ". Error: " + err.Error(),
					})
					if statusErr := r.updateStatusWithRetry(ctx, multiClusterResource); statusErr != nil {
						logger.Error(statusErr, "Failed to update MultiClusterResource status")
					}
					return err
				}
				logger.Info("Resource updated", "name", resource.GetName(), "namespace", resource.GetNamespace(), "cluster", clusterName)
			} else {
				logger.Error(err, "Failed to create resource", "resource", resource.GetName(), "cluster", clusterName)
				meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
					Type:    "Error",
					Status:  metav1.ConditionTrue,
					Reason:  "FailedToCreateResource",
					Message: "Failed to create resource: " + resource.GetName() + " in cluster: " + clusterName + ". Error: " + err.Error(),
				})
				if statusErr := r.updateStatusWithRetry(ctx, multiClusterResource); statusErr != nil {
					logger.Error(statusErr, "Failed to update MultiClusterResource status")
				}
				return err
			}
		} else {
			logger.Info("Resource created", "name", resource.GetName(), "namespace", resource.GetNamespace(), "cluster", clusterName)
		}
	}
	return nil
}

func removeResourcesFromCluster(ctx context.Context, logger logr.Logger, secret corev1.Secret, multiClusterResource *alephatv1.MultiClusterResource) error {
	clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
	kubeconfig := secret.Data["kubeconfig"]
	// Build the client for the target cluster
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		logger.Error(err, "Failed to build REST config for cluster", "cluster", clusterName)
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "Failed to create dynamic client for cluster", "cluster", clusterName)
		return err
	}
	// Delete the resources from the MultiClusterResource in the target cluster
	for _, manifest := range multiClusterResource.Spec.ResourceManifest {
		// Parse the YAML manifest into an unstructured.Unstructured object
		resource := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(manifest), resource); err != nil {
			logger.Error(err, "Failed to unmarshal YAML into unstructured", "manifest", manifest)
			continue
		}
		// Determine if the resource is namespaced or cluster-scoped
		gvr, _ := meta.UnsafeGuessKindToResource(resource.GroupVersionKind())
		if isClusterScopedResource(resource) {
			err = dynamicClient.Resource(gvr).Delete(ctx, resource.GetName(), metav1.DeleteOptions{})
		} else {
			if resource.GetNamespace() == "" {
				resource.SetNamespace("default")
			}
			err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Delete(ctx, resource.GetName(), metav1.DeleteOptions{})
		}
		if err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Failed to delete resource", "resource", resource.GetName(), "cluster", clusterName)
			continue
		} else {
			logger.Info("Resource deleted", "name", resource.GetName(), "namespace", resource.GetNamespace(), "cluster", clusterName)
		}
	}
	return nil
}

func isClusterScopedResource(resource *unstructured.Unstructured) bool {
	// List of known cluster-scoped resources
	clusterScopedResources := map[string]bool{
		"Namespace":                        true,
		"CustomResourceDefinition":         true,
		"ClusterRole":                      true,
		"ClusterRoleBinding":               true,
		"StorageClass":                     true,
		"VolumeAttachment":                 true,
		"Node":                             true,
		"PersistentVolume":                 true,
		"PodSecurityPolicy":                true,
		"PriorityClass":                    true,
		"CSIDriver":                        true,
		"CSINode":                          true,
		"RuntimeClass":                     true,
		"MutatingWebhookConfiguration":     true,
		"ValidatingWebhookConfiguration":   true,
		"ComponentStatus":                  true,
		"ValidatingAdmissionPolicy":        true,
		"ValidatingAdmissionPolicyBinding": true,
		"APIService":                       true,
		"SelfSubjectReview":                true,
		"TokenReview":                      true,
		"SelfSubjectAccessReview":          true,
		"SelfSubjectRulesReview":           true,
		"SubjectAccessReview":              true,
		"CertificateSigningRequest":        true,
		"FlowSchema":                       true,
		"PriorityLevelConfiguration":       true,
		"IngressClass":                     true,
	}

	return clusterScopedResources[resource.GetKind()]
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
