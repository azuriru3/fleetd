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
