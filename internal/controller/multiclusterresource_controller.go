package controller

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	alephatv1 "github.com/moule3053/multiclusterresource/api/v1"
)

const finalizerName = "multicluster.alephat.io/finalizer"

type MultiClusterResourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *MultiClusterResourceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling MultiClusterResource", "namespace", req.Namespace, "name", req.Name)

	// Fetch the MultiClusterResource instance
	multiClusterResource := &alephatv1.MultiClusterResource{}
	err := r.Get(ctx, req.NamespacedName, multiClusterResource)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return. Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			logger.Info("MultiClusterResource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get MultiClusterResource")
		return reconcile.Result{}, err
	}

	// Handle finalizer logic
	if multiClusterResource.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(multiClusterResource, finalizerName) {
			controllerutil.AddFinalizer(multiClusterResource, finalizerName)
			if err := r.Update(ctx, multiClusterResource); err != nil {
				return reconcile.Result{}, err
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(multiClusterResource, finalizerName) {
			// Run finalization logic for multiClusterResource
			if err := r.finalizeMultiClusterResource(ctx, multiClusterResource); err != nil {
				return reconcile.Result{}, err
			}
			controllerutil.RemoveFinalizer(multiClusterResource, finalizerName)
			if err := r.Update(ctx, multiClusterResource); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// List all secrets with prefix "kubeconfig-"
	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList, client.InNamespace("default")); err != nil {
		logger.Error(err, "Failed to list secrets")
		return reconcile.Result{}, err
	}

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

	// Update status to reflect the error if there are missing clusters
	if len(missingClusters) > 0 {
		meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
			Type:    "Error",
			Status:  metav1.ConditionTrue,
			Reason:  "ClusterSecretNotFound",
			Message: "One or more specified clusters do not have corresponding secrets: " + strings.Join(missingClusters, ", "),
		})
	} else {
		meta.RemoveStatusCondition(&multiClusterResource.Status.Conditions, "Error")
	}

	if err := r.updateStatusWithRetry(ctx, multiClusterResource); err != nil {
		logger.Error(err, "Failed to update MultiClusterResource status")
		return reconcile.Result{}, err
	}

	// Create resources in the target clusters that have corresponding secrets
	for _, secret := range secretList.Items {
		if strings.HasPrefix(secret.Name, "kubeconfig-") {
			clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
			if len(targetClusters) > 0 && !contains(targetClusters, clusterName) {
				continue
			}

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
				continue
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
				continue
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
					continue
				}
				// Set the namespace if not specified
				if resource.GetNamespace() == "" {
					resource.SetNamespace("default")
				}
				// Create or update the resource
				gvr, _ := meta.UnsafeGuessKindToResource(resource.GroupVersionKind())
				_, err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Create(ctx, resource, metav1.CreateOptions{})
				if err != nil {
					if errors.IsAlreadyExists(err) {
						_, err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Update(ctx, resource, metav1.UpdateOptions{})
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
							continue
						}
						logger.Info("Resource updated", "timestamp", time.Now().Format(time.RFC3339), "name", resource.GetName(), "namespace", resource.GetNamespace(), "cluster", clusterName)
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
						continue
					}
				} else {
					// Log the creation of the resource
					logger.Info("Resource created", "timestamp", time.Now().Format(time.RFC3339), "name", resource.GetName(), "namespace", resource.GetNamespace(), "cluster", clusterName)
				}
			}
		}
	}

	// Update status with retry logic
	err = r.updateStatusWithRetry(ctx, multiClusterResource)
	if err != nil {
		logger.Error(err, "Failed to update MultiClusterResource status")
		return reconcile.Result{}, err
	}

	logger.Info("Successfully reconciled MultiClusterResource", "namespace", req.Namespace, "name", req.Name)
	return reconcile.Result{}, nil
}

func (r *MultiClusterResourceReconciler) updateStatusWithRetry(ctx context.Context, multiClusterResource *alephatv1.MultiClusterResource) error {
	logger := log.FromContext(ctx)
	for i := 0; i < 5; i++ { // Retry up to 5 times
		err := r.Status().Update(ctx, multiClusterResource)
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
		// Fetch the latest version of the resource and retry
		if err := r.Get(ctx, client.ObjectKeyFromObject(multiClusterResource), multiClusterResource); err != nil {
			return err
		}
		logger.Info("Retrying status update due to conflict", "attempt", i+1)
	}
	return errors.NewConflict(schema.GroupResource{Group: "alephat.alephat.io", Resource: "multiclusterresources"}, multiClusterResource.Name, nil)
}

func (r *MultiClusterResourceReconciler) finalizeMultiClusterResource(ctx context.Context, multiClusterResource *alephatv1.MultiClusterResource) error {
	logger := log.FromContext(ctx)
	logger.Info("Finalizing MultiClusterResource", "namespace", multiClusterResource.Namespace, "name", multiClusterResource.Name)

	// List all secrets with prefix "kubeconfig-"
	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList, client.InNamespace("default")); err != nil {
		logger.Error(err, "Failed to list secrets")
		return err
	}

	var targetClusters []string
	if multiClusterResource.Spec.TargetClusters != "" {
		targetClusters = strings.Split(multiClusterResource.Spec.TargetClusters, ",")
		for i := range targetClusters {
			targetClusters[i] = strings.TrimSpace(targetClusters[i])
		}
	} else {
		for _, secret := range secretList.Items {
			if strings.HasPrefix(secret.Name, "kubeconfig-") {
				clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
				targetClusters = append(targetClusters, clusterName)
			}
		}
	}

	for _, secret := range secretList.Items {
		if strings.HasPrefix(secret.Name, "kubeconfig-") {
			clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
			if len(targetClusters) > 0 && !contains(targetClusters, clusterName) {
				continue
			}

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
				continue
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
				continue
			}
			// Delete the resources from the MultiClusterResource in the target cluster
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
					continue
				}
				// Set the namespace if not specified
				if resource.GetNamespace() == "" {
					resource.SetNamespace("default")
				}
				// Delete the resource
				gvr, _ := meta.UnsafeGuessKindToResource(resource.GroupVersionKind())
				err = dynamicClient.Resource(gvr).Namespace(resource.GetNamespace()).Delete(ctx, resource.GetName(), metav1.DeleteOptions{})
				if err != nil && !errors.IsNotFound(err) {
					logger.Error(err, "Failed to delete resource", "resource", resource.GetName(), "cluster", clusterName)
					meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
						Type:    "Error",
						Status:  metav1.ConditionTrue,
						Reason:  "FailedToDeleteResource",
						Message: "Failed to delete resource: " + resource.GetName() + " in cluster: " + clusterName + ". Error: " + err.Error(),
					})
					if statusErr := r.updateStatusWithRetry(ctx, multiClusterResource); statusErr != nil {
						logger.Error(statusErr, "Failed to update MultiClusterResource status")
					}
					continue
				} else {
					// Log the deletion of the resource
					logger.Info("Resource deleted", "timestamp", time.Now().Format(time.RFC3339), "name", resource.GetName(), "namespace", resource.GetNamespace(), "cluster", clusterName)
				}
			}
		}
	}

	logger.Info("Successfully finalized MultiClusterResource", "namespace", multiClusterResource.Namespace, "name", multiClusterResource.Name)
	return nil
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func (r *MultiClusterResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&alephatv1.MultiClusterResource{}).
		Complete(r)
}
