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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	appsv1alpha1 "github.com/azuriru3/fleetd/api/v1alpha1"
)

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	// fleetdSystemNamespace holds the kubeconfig Secrets for every
	// registered member cluster. Cluster is cluster-scoped, so this is
	// where its spec.kubeconfigSecretRef is looked up from.
	//
	// The manager's RBAC role grants secret read access cluster-wide
	// rather than scoped to this one namespace, since controller-gen
	// generates a single ClusterRole from these markers and doesn't
	// split a per-rule namespace into its own Role. Tightening that is
	// a real improvement but not this stage's job.
	fleetdSystemNamespace = "fleetd-system"
	kubeconfigSecretKey   = "kubeconfig"

	// heartbeatNamespace/heartbeatConfigMapName are where the controller
	// writes a small marker on the member cluster, proving write access
	// alongside the read access proven by listing nodes.
	heartbeatNamespace     = "default"
	heartbeatConfigMapName = "fleetd-heartbeat"

	// heartbeatInterval controls how often a Cluster is re-checked even
	// when nothing about it has changed. Unlike Workload, staying fresh
	// here is the point, so this reconciler always requeues itself.
	heartbeatInterval = 30 * time.Second
)

// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.fleetd.dev,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile connects to the member cluster referenced by a Cluster object,
// reads its nodes to compute allocatable capacity, and writes a heartbeat
// marker back onto that cluster. It requeues itself on a timer so capacity
// and heartbeat data stay fresh even when the Cluster object itself never
// changes.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cluster appsv1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Cluster deleted, nothing to reconcile")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Cluster")
		return ctrl.Result{}, err
	}

	memberClient, err := r.memberClientset(ctx, &cluster)
	if err != nil {
		log.Error(err, "Failed to build client for member cluster", "cluster", cluster.Name)
		return r.setUnready(ctx, &cluster, "ClientBuildFailed", err)
	}

	nodes, err := memberClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Error(err, "Failed to list nodes on member cluster", "cluster", cluster.Name)
		return r.setUnready(ctx, &cluster, "ListNodesFailed", err)
	}

	if err := writeHeartbeat(ctx, memberClient); err != nil {
		log.Error(err, "Failed to write heartbeat to member cluster", "cluster", cluster.Name)
		return r.setUnready(ctx, &cluster, "HeartbeatFailed", err)
	}

	cluster.Status.Allocatable = sumAllocatable(nodes.Items)
	cluster.Status.LastHeartbeatTime = metav1.Now()
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             "Reachable",
		Message:            fmt.Sprintf("observed %d node(s)", len(nodes.Items)),
		ObservedGeneration: cluster.Generation,
	})

	if err := r.Status().Update(ctx, &cluster); err != nil {
		log.Error(err, "Failed to update Cluster status", "cluster", cluster.Name)
		return ctrl.Result{}, err
	}

	log.Info("Reconciled Cluster",
		"cluster", cluster.Name,
		"nodes", len(nodes.Items),
		"allocatable", cluster.Status.Allocatable)
	return ctrl.Result{RequeueAfter: heartbeatInterval}, nil
}

// memberClientset builds a client-go clientset for the member cluster a
// Cluster object refers to, using the kubeconfig stored in the Secret it
// points at. Only core types (Node, ConfigMap) are needed on the member
// side, so a typed clientset is enough; there's no need for a full
// controller-runtime client with fleetd's own scheme registered there.
func (r *ClusterReconciler) memberClientset(ctx context.Context, cluster *appsv1alpha1.Cluster) (*kubernetes.Clientset, error) {
	var secret corev1.Secret
	secretKey := client.ObjectKey{
		Namespace: fleetdSystemNamespace,
		Name:      cluster.Spec.KubeconfigSecretRef.Name,
	}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		return nil, fmt.Errorf("get kubeconfig secret %s: %w", secretKey, err)
	}

	kubeconfig, ok := secret.Data[kubeconfigSecretKey]
	if !ok {
		return nil, fmt.Errorf("secret %s has no %q key", secretKey, kubeconfigSecretKey)
	}

	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig from secret %s: %w", secretKey, err)
	}

	return kubernetes.NewForConfig(restCfg)
}

// sumAllocatable adds up allocatable cpu and memory across every node in a
// cluster. This is the read proof: it only means anything if the
// controller actually reached the member cluster's API server.
func sumAllocatable(nodes []corev1.Node) corev1.ResourceList {
	total := corev1.ResourceList{}
	for _, node := range nodes {
		for name, qty := range node.Status.Allocatable {
			if existing, ok := total[name]; ok {
				existing.Add(qty)
				total[name] = existing
			} else {
				total[name] = qty.DeepCopy()
			}
		}
	}
	return total
}

// writeHeartbeat upserts a small ConfigMap on the member cluster. This is
// the write proof, kept deliberately separate from real workload
// placement, which is not this stage's job.
func writeHeartbeat(ctx context.Context, memberClient kubernetes.Interface) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      heartbeatConfigMapName,
			Namespace: heartbeatNamespace,
		},
		Data: map[string]string{
			"lastSeen": time.Now().UTC().Format(time.RFC3339),
		},
	}

	configMaps := memberClient.CoreV1().ConfigMaps(heartbeatNamespace)
	_, err := configMaps.Get(ctx, heartbeatConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = configMaps.Create(ctx, cm, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	default:
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	}
}

// setUnready records why a Cluster could not be reconciled and still
// requeues, since a currently-unreachable cluster may come back.
func (r *ClusterReconciler) setUnready(ctx context.Context, cluster *appsv1alpha1.Cluster, reason string, cause error) (ctrl.Result, error) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: cluster.Generation,
	})
	if err := r.Status().Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: heartbeatInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// GenerationChangedPredicate filters the watch down to spec changes and
// creation/deletion, ignoring status-only updates. Without it, every
// status write this reconciler makes (LastHeartbeatTime always changes)
// would immediately re-trigger itself through the watch, on top of the
// heartbeatInterval requeue below, doubling every reconcile into a pair
// fired back to back.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("cluster").
		Complete(r)
}
