/*
Copyright 2026 Azril.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/azuriru3/fleetd/api/v1alpha1"
)

// WorkloadReconciler reconciles a Workload object
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const readyConditionType = "Ready"

// localClusterName is a placeholder cluster identity. There is only one
// cluster to place workloads on at this stage, so assignment is fixed.
// Stage 3 replaces this with a real cluster registry to choose from.
const localClusterName = "local"

// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=workloads/finalizers,verbs=update

// Reconcile assigns the Workload to the single available cluster and marks
// it Ready. It only writes status when something actually changed.
func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var wl appsv1alpha1.Workload
	if err := r.Get(ctx, req.NamespacedName, &wl); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Workload deleted, nothing to reconcile")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Workload")
		return ctrl.Result{}, err
	}

	assignmentChanged := wl.Status.AssignedCluster != localClusterName
	wl.Status.AssignedCluster = localClusterName

	conditionChanged := meta.SetStatusCondition(&wl.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             "Placed",
		Message:            fmt.Sprintf("assigned to cluster %q", localClusterName),
		ObservedGeneration: wl.Generation,
	})

	if !assignmentChanged && !conditionChanged {
		log.Info("Workload already up to date", "name", wl.Name)
		return ctrl.Result{}, nil
	}

	if err := r.Status().Update(ctx, &wl); err != nil {
		log.Error(err, "Failed to update Workload status", "name", wl.Name)
		return ctrl.Result{}, err
	}

	log.Info("Reconciled Workload",
		"name", wl.Name,
		"priority", wl.Spec.Priority,
		"assignedCluster", wl.Status.AssignedCluster)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Workload{}).
		Named("workload").
		Complete(r)
}
