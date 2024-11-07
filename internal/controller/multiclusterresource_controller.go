package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
			logger.Info("MultiClusterResource not found. Ignoring since object must be deleted.")
			return reconcile.Result{}, nil
		}
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

	targetClusters, missingClusters := getTargetClusters(multiClusterResource, secretList)

	// Update status to reflect the error if there are missing clusters
	if len(missingClusters) > 0 {
		logger.Error(nil, "One or more specified clusters do not have corresponding secrets", "clusters", missingClusters)
		meta.SetStatusCondition(&multiClusterResource.Status.Conditions, metav1.Condition{
			Type:    "Error",
			Status:  metav1.ConditionTrue,
			Reason:  "ClusterSecretNotFound",
			Message: "One or more specified clusters do not have corresponding secrets: " + strings.Join(missingClusters, ", "),
		})
	} else {
		meta.RemoveStatusCondition(&multiClusterResource.Status.Conditions, "Error")
	}

	deployedClusters := []string{}

	// Create resources in the target clusters that have corresponding secrets
	for _, secret := range secretList.Items {
		if strings.HasPrefix(secret.Name, "kubeconfig-") {
			clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
			if len(targetClusters) > 0 && !contains(targetClusters, clusterName) {
				continue
			}

			if err := applyResourcesToCluster(ctx, logger, secret, multiClusterResource, r); err != nil {
				continue
			}
			deployedClusters = append(deployedClusters, clusterName)
		}
	}

	// Remove resources from clusters that are no longer in targetClusters
	for _, secret := range secretList.Items {
		if strings.HasPrefix(secret.Name, "kubeconfig-") {
			clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
			if len(targetClusters) > 0 && contains(targetClusters, clusterName) {
				continue
			}

			if err := removeResourcesFromCluster(ctx, logger, secret, multiClusterResource); err != nil {
				continue
			}
		}
	}

	// Update the status with the list of deployed clusters
	multiClusterResource.Status.DeployedClusters = deployedClusters

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
	return errors.NewConflict(schema.GroupResource{Group: "alephat.io", Resource: "multiclusterresources"}, multiClusterResource.Name, nil)
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

	targetClusters, _ := getTargetClusters(multiClusterResource, secretList)

	for _, secret := range secretList.Items {
		if strings.HasPrefix(secret.Name, "kubeconfig-") {
			clusterName := strings.TrimPrefix(secret.Name, "kubeconfig-")
			if len(targetClusters) > 0 && !contains(targetClusters, clusterName) {
				continue
			}

			if err := removeResourcesFromCluster(ctx, logger, secret, multiClusterResource); err != nil {
				continue
			}
		}
	}

	logger.Info("Successfully finalized MultiClusterResource", "namespace", multiClusterResource.Namespace, "name", multiClusterResource.Name)
	return nil
}

func (r *MultiClusterResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&alephatv1.MultiClusterResource{}).
		Complete(r)
}
