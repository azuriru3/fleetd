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
