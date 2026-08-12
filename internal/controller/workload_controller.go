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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/azuriru3/fleetd/api/v1alpha1"
)

// WorkloadReconciler reconciles a Workload object
type WorkloadReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const readyConditionType = "Ready"

// unplacedRetryInterval is how often an unplaced Workload gets
// re-evaluated even without any new event. It's a safety net, not the
// primary trigger: deleting a sibling Workload frees capacity without
// touching the Cluster watch below, so this is what catches that case.
const unplacedRetryInterval = 15 * time.Second

// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=workloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=workloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=workloads/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=clusters,verbs=get;list;watch

// Reconcile places a Workload on the best-fitting registered Cluster and
// marks it Ready. Placement is sticky: once a Workload has an
// assignedCluster, this stage never moves or re-scores it. If nothing
// fits, the Workload is left unplaced and retried later rather than
// treated as an error.
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

	if wl.Status.AssignedCluster != "" {
		log.Info("Workload already placed, leaving as is", "name", wl.Name, "assignedCluster", wl.Status.AssignedCluster)
		return ctrl.Result{}, nil
	}

	winner, err := r.pickCluster(ctx, &wl)
	if err != nil {
		log.Error(err, "Failed to evaluate clusters for Workload", "name", wl.Name)
		return ctrl.Result{}, err
	}

	if winner == "" {
		changed := meta.SetStatusCondition(&wl.Status.Conditions, metav1.Condition{
			Type:               readyConditionType,
			Status:             metav1.ConditionFalse,
			Reason:             "NoCapacity",
			Message:            "no ready cluster currently has enough allocatable capacity for this workload",
			ObservedGeneration: wl.Generation,
		})
		if changed {
			if err := r.Status().Update(ctx, &wl); err != nil {
				log.Error(err, "Failed to update Workload status", "name", wl.Name)
				return ctrl.Result{}, err
			}
		}
		log.Info("Workload could not be placed", "name", wl.Name, "priority", wl.Spec.Priority)
		return ctrl.Result{RequeueAfter: unplacedRetryInterval}, nil
	}

	wl.Status.AssignedCluster = winner
	meta.SetStatusCondition(&wl.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             "Placed",
		Message:            fmt.Sprintf("assigned to cluster %q", winner),
		ObservedGeneration: wl.Generation,
	})

	if err := r.Status().Update(ctx, &wl); err != nil {
		log.Error(err, "Failed to update Workload status", "name", wl.Name)
		return ctrl.Result{}, err
	}

	log.Info("Reconciled Workload",
		"name", wl.Name,
		"priority", wl.Spec.Priority,
		"assignedCluster", winner)
	return ctrl.Result{}, nil
}

// pickCluster scores every Ready cluster that has room for wl's requested
// resources and returns the name of the tightest fit: the candidate that
// would have the least cpu left over after placement. This is a
// bin-packing strategy, favoring utilization density over spreading load
// evenly. It returns an empty string, not an error, when nothing fits.
func (r *WorkloadReconciler) pickCluster(ctx context.Context, wl *appsv1alpha1.Workload) (string, error) {
	var clusters appsv1alpha1.ClusterList
	if err := r.List(ctx, &clusters); err != nil {
		return "", fmt.Errorf("list clusters: %w", err)
	}

	var workloads appsv1alpha1.WorkloadList
	if err := r.List(ctx, &workloads); err != nil {
		return "", fmt.Errorf("list workloads: %w", err)
	}
	consumed := sumRequestsByCluster(workloads.Items)

	requested := wl.Spec.Resources.Requests

	var best string
	var bestRemainingCPU resource.Quantity
	for _, c := range clusters.Items {
		if !meta.IsStatusConditionTrue(c.Status.Conditions, readyConditionType) {
			continue
		}

		remaining := subtractResources(c.Status.Allocatable, consumed[c.Name])
		if !fits(requested, remaining) {
			continue
		}

		afterPlacement := subtractResources(remaining, requested)
		cpuAfter := afterPlacement[corev1.ResourceCPU]
		if best == "" || cpuAfter.Cmp(bestRemainingCPU) < 0 {
			best = c.Name
			bestRemainingCPU = cpuAfter
		}
	}
	return best, nil
}

// sumRequestsByCluster adds up the resource requests of every already
// placed Workload, grouped by which cluster it landed on. An unplaced
// Workload (assignedCluster empty) contributes nothing.
func sumRequestsByCluster(workloads []appsv1alpha1.Workload) map[string]corev1.ResourceList {
	sums := map[string]corev1.ResourceList{}
	for _, wl := range workloads {
		if wl.Status.AssignedCluster == "" {
			continue
		}
		existing := sums[wl.Status.AssignedCluster]
		if existing == nil {
			existing = corev1.ResourceList{}
		}
		for name, qty := range wl.Spec.Resources.Requests {
			total := existing[name]
			total.Add(qty)
			existing[name] = total
		}
		sums[wl.Status.AssignedCluster] = existing
	}
	return sums
}

// subtractResources returns a - b, per resource name. A name missing from
// either side is treated as zero.
func subtractResources(a, b corev1.ResourceList) corev1.ResourceList {
	result := corev1.ResourceList{}
	for name, qty := range a {
		result[name] = qty.DeepCopy()
	}
	for name, qty := range b {
		remaining := result[name]
		remaining.Sub(qty)
		result[name] = remaining
	}
	return result
}

// fits reports whether remaining has enough of every resource requested.
// A resource missing from requested needs nothing and always fits.
func fits(requested, remaining corev1.ResourceList) bool {
	for name, need := range requested {
		have := remaining[name]
		if have.Cmp(need) < 0 {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
//
// The Watches call means a Cluster becoming Ready, or its capacity
// changing, immediately re-evaluates every currently unplaced Workload
// instead of waiting for unplacedRetryInterval to poll again.
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Workload{}).
		Watches(
			&appsv1alpha1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.unplacedWorkloadsForClusterEvent),
		).
		Named("workload").
		Complete(r)
}

// unplacedWorkloadsForClusterEvent maps any Cluster event to reconcile
// requests for every Workload that has not been placed yet.
func (r *WorkloadReconciler) unplacedWorkloadsForClusterEvent(ctx context.Context, _ client.Object) []reconcile.Request {
	var workloads appsv1alpha1.WorkloadList
	if err := r.List(ctx, &workloads); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, wl := range workloads.Items {
		if wl.Status.AssignedCluster == "" {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&wl)})
		}
	}
	return requests
}
