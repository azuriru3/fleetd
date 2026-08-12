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
	"slices"
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

	// APIReader is a direct, uncached read against the API server, used
	// only for the capacity math in pickCluster. Client is backed by an
	// informer cache, which is fine for Get-ing the object a reconcile
	// was triggered for, but scheduling decisions read OTHER objects
	// too - and a scheduling decision made moments after a sibling
	// Workload's own reconcile just wrote to several different objects
	// can observe a cache snapshot that hasn't caught up with those
	// writes yet. That caused a real overcommit: a just-evicted
	// Workload re-placed itself using a stale view of what was still
	// using the cluster's capacity. APIReader always reads live.
	APIReader client.Reader
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

	winner, evicted, err := r.pickCluster(ctx, &wl)
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

	if len(evicted) > 0 {
		log.Info("Reconciled Workload via preemption",
			"name", wl.Name,
			"priority", wl.Spec.Priority,
			"assignedCluster", winner,
			"evicted", evicted)
	} else {
		log.Info("Reconciled Workload",
			"name", wl.Name,
			"priority", wl.Spec.Priority,
			"assignedCluster", winner)
	}
	return ctrl.Result{}, nil
}

// pickCluster finds a home for wl. It first tries every Ready cluster for
// a normal bin-pack fit, exactly as Stage 4 did. Only if nothing fits
// without disturbing anyone does it consider preemption: evicting some of
// a candidate cluster's strictly-lower-priority residents to make room.
// It returns the winning cluster and the names of anything evicted to
// make that placement possible (nil if no eviction was needed). An empty
// winner, not an error, means nothing fit even with preemption.
func (r *WorkloadReconciler) pickCluster(ctx context.Context, wl *appsv1alpha1.Workload) (string, []string, error) {
	var clusters appsv1alpha1.ClusterList
	if err := r.APIReader.List(ctx, &clusters); err != nil {
		return "", nil, fmt.Errorf("list clusters: %w", err)
	}

	var workloads appsv1alpha1.WorkloadList
	if err := r.APIReader.List(ctx, &workloads); err != nil {
		return "", nil, fmt.Errorf("list workloads: %w", err)
	}
	placed := groupPlacedByCluster(workloads.Items)
	requested := wl.Spec.Resources.Requests

	var readyClusters []appsv1alpha1.Cluster
	for _, c := range clusters.Items {
		if meta.IsStatusConditionTrue(c.Status.Conditions, readyConditionType) {
			readyClusters = append(readyClusters, c)
		}
	}

	if winner := binPack(readyClusters, placed, requested); winner != "" {
		return winner, nil, nil
	}

	bestCluster, bestVictims := bestPreemptionCandidate(readyClusters, placed, wl.Spec.Priority, requested)
	if bestCluster == "" {
		return "", nil, nil
	}

	names := make([]string, 0, len(bestVictims))
	for i := range bestVictims {
		if err := r.evict(ctx, &bestVictims[i], wl.Name); err != nil {
			return "", nil, fmt.Errorf("evict %s: %w", bestVictims[i].Name, err)
		}
		names = append(names, bestVictims[i].Name)
	}
	return bestCluster, names, nil
}

// binPack returns the Ready cluster with the least cpu remaining after
// placing requested, among clusters that already have room without
// evicting anyone. Empty string if none fit.
func binPack(clusters []appsv1alpha1.Cluster, placed map[string][]appsv1alpha1.Workload, requested corev1.ResourceList) string {
	var best string
	var bestRemainingCPU resource.Quantity
	for _, c := range clusters {
		remaining := subtractResources(c.Status.Allocatable, sumRequests(placed[c.Name]))
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
	return best
}

// bestPreemptionCandidate evaluates every Ready cluster for whether
// evicting some of its strictly-lower-priority residents would make room,
// and returns whichever candidate needs the fewest evictions. Ties are
// broken the same way as binPack: least cpu remaining after placement.
func bestPreemptionCandidate(clusters []appsv1alpha1.Cluster, placed map[string][]appsv1alpha1.Workload, preemptorPriority int32, requested corev1.ResourceList) (string, []appsv1alpha1.Workload) {
	var bestCluster string
	var bestVictims []appsv1alpha1.Workload
	var bestRemainingCPU resource.Quantity

	for _, c := range clusters {
		victims, remainingAfterEviction, ok := planEviction(c.Status.Allocatable, placed[c.Name], preemptorPriority, requested)
		if !ok {
			continue
		}
		afterPlacement := subtractResources(remainingAfterEviction, requested)
		cpuAfter := afterPlacement[corev1.ResourceCPU]

		better := bestCluster == "" ||
			len(victims) < len(bestVictims) ||
			(len(victims) == len(bestVictims) && cpuAfter.Cmp(bestRemainingCPU) < 0)
		if better {
			bestCluster = c.Name
			bestVictims = victims
			bestRemainingCPU = cpuAfter
		}
	}
	return bestCluster, bestVictims
}

// planEviction decides whether evicting some subset of residents (only
// those strictly lower priority than preemptorPriority) would free enough
// room for requested. Residents are evicted lowest-priority-first,
// stopping as soon as there's enough room - a greedy choice, not an
// optimal one, made to keep this simple rather than solving a knapsack
// problem to minimize disruption. Returns the workloads to evict, the
// cluster's remaining capacity after those evictions, and whether a fit
// was found at all.
func planEviction(allocatable corev1.ResourceList, residents []appsv1alpha1.Workload, preemptorPriority int32, requested corev1.ResourceList) ([]appsv1alpha1.Workload, corev1.ResourceList, bool) {
	evictable := make([]appsv1alpha1.Workload, 0, len(residents))
	for _, wl := range residents {
		if wl.Spec.Priority < preemptorPriority {
			evictable = append(evictable, wl)
		}
	}
	slices.SortFunc(evictable, func(a, b appsv1alpha1.Workload) int {
		return int(a.Spec.Priority) - int(b.Spec.Priority)
	})

	remaining := subtractResources(allocatable, sumRequests(residents))
	var victims []appsv1alpha1.Workload
	for _, candidate := range evictable {
		if fits(requested, remaining) {
			break
		}
		remaining = addResources(remaining, candidate.Spec.Resources.Requests)
		victims = append(victims, candidate)
	}
	if !fits(requested, remaining) {
		return nil, nil, false
	}
	return victims, remaining, true
}

// evict clears a victim's placement and marks it Preempted. It re-fetches
// the victim first rather than trusting the copy from an earlier List
// call, since that snapshot may be stale by the time this write happens.
func (r *WorkloadReconciler) evict(ctx context.Context, victim *appsv1alpha1.Workload, preemptor string) error {
	var fresh appsv1alpha1.Workload
	if err := r.Get(ctx, client.ObjectKeyFromObject(victim), &fresh); err != nil {
		return err
	}

	fresh.Status.AssignedCluster = ""
	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             "Preempted",
		Message:            fmt.Sprintf("evicted to make room for higher priority workload %q", preemptor),
		ObservedGeneration: fresh.Generation,
	})
	return r.Status().Update(ctx, &fresh)
}

// groupPlacedByCluster groups already-placed Workloads by which cluster
// they landed on. An unplaced Workload (assignedCluster empty) is never
// included, so the Workload currently being reconciled never appears
// here even though it's in the same List result.
func groupPlacedByCluster(workloads []appsv1alpha1.Workload) map[string][]appsv1alpha1.Workload {
	groups := map[string][]appsv1alpha1.Workload{}
	for _, wl := range workloads {
		if wl.Status.AssignedCluster == "" {
			continue
		}
		groups[wl.Status.AssignedCluster] = append(groups[wl.Status.AssignedCluster], wl)
	}
	return groups
}

// sumRequests adds up the resource requests across a set of Workloads.
func sumRequests(workloads []appsv1alpha1.Workload) corev1.ResourceList {
	total := corev1.ResourceList{}
	for _, wl := range workloads {
		for name, qty := range wl.Spec.Resources.Requests {
			existing := total[name]
			existing.Add(qty)
			total[name] = existing
		}
	}
	return total
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

// addResources returns a + b, per resource name.
func addResources(a, b corev1.ResourceList) corev1.ResourceList {
	result := corev1.ResourceList{}
	for name, qty := range a {
		result[name] = qty.DeepCopy()
	}
	for name, qty := range b {
		total := result[name]
		total.Add(qty)
		result[name] = total
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
