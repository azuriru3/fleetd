# Design

## Reconciliation and Idempotency

The HelloWorld controller follows the normal Kubernetes reconciliation pattern. The `HelloWorld` resource contains the desired value in `spec.foo`, while the controller reports what it observed through `status.conditions`.

When a `HelloWorld` object changes, the reconciler runs. It first gets the latest version of the object from the API server. This is important because the reconcile request only tells us which object needs attention. It does not contain the current state we should work with.

The controller then builds the greeting from `spec.foo` and updates the `Ready` condition in the status. The status is only written when the condition actually needs to change.

There is an important behavior here that is easy to miss. Updating the status is itself a change to the `HelloWorld` object. Since the controller watches `HelloWorld` objects, that status update causes another reconcile.

We saw this happen in the kind cluster. After applying `foo=fleetd`, the controller reconciled the object and updated its status. Kubernetes then triggered another reconcile automatically. The second run reported that the object was already up to date and made no further change.

The same thing happened after changing `foo` to `staff-infra`. The spec change caused a reconcile, the controller updated the status, and that status update caused another reconcile. The follow-up run then did nothing because the desired status was already correct.

This is why the reconciler needs to be idempotent. Reconcile may run more than once for the same object, and the controller should safely reach the same result without continuously changing the resource.

The delete case follows the same principle. When the object no longer exists, the API server returns `NotFound`. The controller treats this as normal and exits cleanly instead of reporting an error or crashing.

In simple terms, the controller works like this:

1. Get the latest object.
2. If it no longer exists, stop.
3. Work out what the status should be.
4. Update the status only if it is different.
5. If that update causes another reconcile, the next run sees that everything is already correct and does nothing.

The important design idea is not that Reconcile runs only when something changes. It is that Reconcile is safe to run repeatedly. That makes the controller reliable after status updates, repeated events, restarts, or any other situation where Kubernetes asks it to reconcile the same object again.

## Workload as Placement Intent

Stage 2 introduces the `Workload` resource as a simple representation of work that needs to be placed somewhere.

The main idea is to separate **what should run** from **where it should run**.

The `spec` contains information that describes the workload itself. `priority` is a simple integer that can be used later when workloads need to be scored or when higher-priority work needs to preempt lower-priority work.

`resources` uses Kubernetes' existing `corev1.ResourceRequirements` type instead of creating a new resource format. This is intentional. It uses the same structure that containers already use for CPU and memory requirements, so a later controller can take this information and use it directly when creating an actual workload on a cluster.

Placement is kept in the status for now. `status.assignedCluster` is empty until something places the workload. At this stage there is only one cluster, so the reconciler assigns it to `local`. This is deliberately simple. The placement logic will become more meaningful once multiple clusters exist.

The `Ready` condition now represents something more specific than simply saying the controller has processed the object. It means the workload has actually been assigned. A successful placement results in `Ready=True` with the reason `Placed`.

### Why placement constraints are not here yet

A placement constraint would need something to select against. For example, a constraint might say that a workload should run on a cluster with a particular label.

That concept does not exist yet because the `Cluster` resource is only introduced in the next stage. Adding placement constraints now would require inventing the shape of a cluster before the cluster itself has been designed.

`priority` does not have this problem. It is just a number and does not depend on another resource existing.

### Why the status subresource matters

The `Workload` resource uses Kubernetes' status subresource.

This separates `spec` and `status` at the API level. A write to the main resource cannot change the status, while a write to `/status` cannot change the spec.

This gives ownership to the two sides of the controller:

* The user or another tool controls the desired state in `spec`.
* The controller controls the observed state in `status`.

Without this separation, both sides would be working with the whole object when making changes. A simple read-modify-write could then accidentally overwrite a newer change made by the other side.

### Why create Workload instead of using Deployment directly?

A Kubernetes `Deployment` already has a desired state, a controller, and status. There is no reason for `Workload` to recreate those concepts.

The important difference is placement.

A Deployment belongs to the Kubernetes cluster where it is created. It does not represent something that is waiting to be assigned to one cluster out of several.

`Workload` fills that gap.

It represents the intent before the final placement decision:

> This workload needs these resources, has this priority, and has not yet been assigned to a cluster.

Later stages can use this information to evaluate available clusters, make a placement decision, and eventually create a real `Deployment` on the selected cluster.

So the `Workload` is the **placement intent and decision record**, while a future `Deployment` becomes the **execution artifact**.

### Reconciliation behavior

The reconciliation behavior remains consistent with Stage 1.

When the sample workload was created, the controller assigned it to `local` and reported `Ready=True` with reason `Placed`.

Changing the priority from `10` to `90` caused the resource to be reconciled again. The observed generation changed, the controller processed the new state, and the follow-up reconciliation settled into a no-op.

Deleting the workload was also handled cleanly through the existing `NotFound` path.

The important point is that adding more fields did not change the basic controller design. The object still represents desired state, the controller observes that state, and status records what the controller has actually decided.

## Cluster Registration and Hub-and-Spoke Communication

Stage 3 introduces the `Cluster` resource. Unlike `Workload`, a `Cluster` is cluster-scoped because a registered Kubernetes cluster is not part of a particular namespace. This follows the same general model Kubernetes uses for resources such as `Node` and `StorageClass`.

The `Cluster` object represents a cluster that fleetd can communicate with.

### Credentials stay in a Secret

The `Cluster` spec does not contain the kubeconfig itself. Instead, `spec.kubeconfigSecretRef` points to a Kubernetes Secret containing the credentials.

This keeps credentials separate from the cluster registration object. Someone can inspect the `Cluster` resource with `kubectl get -o yaml` without exposing the credentials needed to access the member cluster.

The object also does not have a separate `spec.labels` field. Kubernetes objects already have `metadata.labels`, so adding another labeling mechanism would duplicate something Kubernetes already provides.

### Observed cluster state

The controller records information it actually observes in status.

`status.allocatable` contains the resources available on the member cluster. The controller gets this information by connecting to the cluster and reading its Nodes. This is observed state, not something a user specifies.

This is important for the next stage because the scoring logic will need to know how much CPU, memory, and other resources each cluster can provide.

`status.lastHeartbeatTime` serves a different purpose. It shows that fleetd was able to successfully communicate with the cluster. The resource information and the heartbeat are therefore separate signals. A cluster could have resource information recorded, while the heartbeat tells us whether communication is still working now.

### Hub-and-spoke model

The design uses a hub-and-spoke model.

fleetd runs on the main cluster and connects directly to each registered member cluster using the kubeconfig stored in the referenced Secret. The member clusters do not need to run another fleetd agent that reports information back to the hub.

This is similar to the architecture used by systems such as Karmada and Admiralty.

An agent-based model can be more practical at larger scale, especially when member clusters are behind NAT or cannot accept connections from the hub. However, it would also mean building, deploying, and maintaining another binary.

For this stage, the hub connecting directly to the member clusters is simpler and is enough to prove the core behavior.

### Read and write access are tested separately

Successfully listing Nodes proves that fleetd can authenticate to the member cluster and read information.

It does not prove that fleetd can actually create or modify resources.

That distinction matters because a later stage will need fleetd to create real workloads on member clusters. Therefore, this stage also writes a heartbeat ConfigMap to the member cluster.

The read and write paths were verified independently by connecting to the member clusters with their own kubeconfigs and checking that the ConfigMap actually existed. This avoids relying only on fleetd's own status to prove that the operation succeeded.

## Avoiding Reconcile Loops

The Cluster controller exposed an important problem with watching resources that the controller itself updates.

Initially, the controller watched all updates to `Cluster`. Every reconcile updated `status.lastHeartbeatTime`. Since that status update was itself an update to the `Cluster` object, the watch immediately triggered another reconcile.

This created pairs of reconciles. The logs showed two reconciles at almost the same time, such as `12:06:24` twice and `12:06:57` twice.

There was also a `RequeueAfter` timer configured to run the heartbeat again after roughly 30 seconds. This meant the controller could trigger another reconcile through two different paths:

1. The heartbeat timer.
2. Its own status update.

The problem is different from Stage 1 and Stage 2. Those controllers only update status when the status actually changes. The heartbeat is intentionally different because `lastHeartbeatTime` is supposed to change on every successful heartbeat.

The fix was to add `predicate.GenerationChangedPredicate` to the Cluster watch.

The generation of a Kubernetes object changes when its spec changes, but not when the controller only changes its status. The predicate therefore allows creation and spec changes to trigger reconciliation while ignoring status-only updates made by the controller itself.

The `RequeueAfter` timer still works normally because it is an internal workqueue requeue, not an object update event. The predicate does not block it.

After the fix, the logs showed single reconciles roughly 33 seconds apart, such as `12:09:09`, `12:09:42`, and `12:10:15`. The duplicate reconciles were gone.

The general lesson is that a controller needs to be careful about what events it watches when it also writes to the same resource. A status update should not accidentally become an endless source of its own reconciliation events.

## Known RBAC Limitation

There is currently a known RBAC gap.

The intended design was for the controller to read Secrets only from the `fleetd-system` namespace. However, the generated RBAC rules placed that permission in the same cluster-wide `ClusterRole` used by the controller.

As a result, the manager can currently read Secrets across the cluster, which is broader access than intended.

This does not prevent the current design from working, but it should be tightened before treating the RBAC configuration as production-ready.
