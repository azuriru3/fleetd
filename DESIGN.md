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

## Placement and Scoring

Stage 4 introduces the first real placement decision.

When a `Workload` needs to be placed, the controller looks at the registered clusters and decides which one should receive it. The controller does not simply look at the raw capacity of each cluster. It first works out how much capacity is already being used.

The process is:

1. Find all clusters that are `Ready`.
2. Find all existing `Workload` objects and group their resource requests by `status.assignedCluster`.
3. Subtract the consumed resources from each cluster's allocatable resources.
4. Remove clusters that do not have enough remaining capacity for the new workload.
5. From the remaining candidates, choose the cluster that would have the **least CPU remaining** after placing the workload.

This means the decision is based on actual remaining capacity rather than just the total capacity reported by the cluster.

An unreachable or unhealthy cluster is not considered for placement. The controller uses the cluster's `Ready` condition as the first filter, so a cluster that is not ready cannot become a placement candidate.

## Bin-Packing Strategy

The scoring strategy implemented here is **bin-packing**.

In simple terms, the controller tries to fill the clusters that are already more heavily used before using an empty cluster.

For example, a 6 CPU workload was placed on `member-b`. When another 1 CPU workload was created, it was also placed on `member-b` even though `member-a` was empty.

That is direct evidence of the bin-packing behavior.

The alternative is **spreading**, where workloads are distributed more evenly across available clusters.

Both strategies have valid uses:

* Bin-packing improves utilization by concentrating workloads and leaving other capacity available.
* Spreading provides more isolation because workloads are distributed across more clusters.

Bin-packing was chosen here because it also creates the conditions needed for the next stage. Preemption becomes meaningful when clusters actually become full. Filling available capacity is therefore a natural fit for demonstrating preemption later.

Spread is not implemented yet. This is intentional. Proving one real scoring strategy from end to end is more useful at this stage than adding a configuration option for another strategy that has not been tested.

## CPU Is the Only Scored Resource

The workload still contains Kubernetes resource requirements, but only CPU is used to rank clusters against each other.

Eligibility is checked against every resource the workload actually requests. If a workload asks for more memory than a cluster has remaining, that cluster is filtered out in step 4 regardless of CPU. So a workload is never placed onto a cluster that cannot actually fit it.

What is CPU-only is the tiebreak in step 5. Among clusters that already passed the eligibility check, the one with the least CPU remaining after placement wins. Memory is not used to break that tie.

A real scheduler would normally rank across multiple dimensions, potentially with different weights. That is not necessary to prove the mechanism here. CPU alone is enough to demonstrate ranking behavior. Adding more scoring dimensions to the ranking step would add complexity without another stage currently depending on it.

## Placement Is Sticky

Once a workload has been assigned to a cluster, the controller does not continuously reconsider the decision.

Re-evaluating every existing workload whenever cluster capacity changes would introduce a different feature: **rebalancing**.

Rebalancing means deciding whether moving an already-running workload to another cluster is worth the disruption caused by moving it. That is a separate problem from deciding where a new workload should go.

For this reason, initial placement and rebalancing are kept separate. The current stage handles the first placement only. Rebalancing can be added later if there is a reason to need it.

## Priority Is Not Used for Placement Yet

`spec.priority` already exists and is logged, but the placement algorithm does not currently use it.

This is deliberate, because simply reading the priority inside `pickCluster` would not actually create priority-aware scheduling.

Kubernetes controllers process reconciliation work through their workqueue based on when work arrives. They do not automatically process objects according to a `priority` field.

For example, if a priority-10 workload and a priority-90 workload arrive at roughly the same time and there is only enough capacity for one of them, whichever reconciliation happens first can claim the available capacity. The higher-priority workload does not automatically jump ahead.

True priority-aware placement would require a priority-aware queue or another mechanism for coordinating competing workloads.

There is another way to handle this. The controller can allow the initial placement to happen, then detect that a higher-priority workload needs the capacity and preempt the lower-priority workload.

That is the approach intended for Stage 5.

So priority is already part of the data model, but its actual scheduling effect is intentionally deferred to preemption.

## Reacting to Available Capacity

The placement controller also needs to react when a cluster becomes available again.

This was tested by deregistering both clusters and creating a workload that could not be placed. After registering one cluster again, the workload was placed in about **0.63 seconds**.

This is important because the controller also has a 15-second polling interval. A placement that happened in less than a second could not have been caused by waiting for the next poll.

The result proves that the cluster watch is triggering the reconciliation when cluster state changes.

The controller therefore has two useful behaviors:

* It periodically checks for placement opportunities.
* It reacts directly when relevant cluster changes happen.

## No Capacity

If no Ready cluster has enough remaining capacity for the requested workload, the workload is left unplaced and the controller reports that there is no capacity.

A 999 CPU workload was tested and correctly returned `NoCapacity`.

This is an important part of the design because placement is not guaranteed. The scheduler should not assign a workload to a cluster simply because it needs somewhere to go. The selected cluster must actually have enough remaining capacity.
