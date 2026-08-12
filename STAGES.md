# fleetd - build stages

Rules for how this gets built, stage by stage, so scope stays tight and nothing grows before its time.

## Stage 0: scaffold and tooling (done)

Building: kubebuilder project init, a HelloWorld sample API and controller wired to a manager, dev tooling (go, kubectl, kind, helm, terraform, kubebuilder) installed and working, repo on a real filesystem path.

Not doing: any real reconcile logic yet, no design decisions locked in.

## Stage 1: reconcile loop, for real

Building: give the HelloWorld controller actual logic (read spec, write a status condition), spin up a local kind cluster, create/update/delete a HelloWorld object and watch the controller react in real time.

Not doing: the real Workload API, multiple clusters, any scheduling logic. This stage exists purely to get comfortable with reconcile loop mechanics (watches, requeues, idempotency) before the real scope starts.

Exit: DESIGN.md has a section explaining what the reconcile loop does and why it's shaped the way it is, written in plain language after actually watching it run.

## Stage 2: the real Workload API, single cluster

Building: drop HelloWorld, define the actual Workload CRD (priority, resource requests, placement constraints in spec; assigned cluster and conditions in status). Controller reconciles against one cluster only. No scoring, no multi-cluster, just prove the API shape and reconcile flow end to end.

Not doing: multiple clusters, scoring, preemption.

Exit: DESIGN.md section on the API design. why these fields, why a status subresource, how this compares to just using a plain Deployment.

## Stage 3: multi-cluster registration

Building: a way to register more than one cluster (a Cluster resource or config holding capacity and labels), and give the controller the ability to read and write across multiple kubeconfigs at once.

Not doing: any placement decision logic yet. This stage only proves the controller can see and act across N clusters, not that it picks the right one.

Exit: DESIGN.md section on how multi-cluster client management works, and the tradeoffs of one controller reaching out to N clusters vs N agents reporting in to one controller.

## Stage 4: placement and scoring

Building: the actual scheduling decision. Score each candidate cluster (bin-pack density, spread, locality) and place the Workload on the winner.

Not doing: preemption. If there's no room, the workload just doesn't get placed yet.

Exit: DESIGN.md tradeoff writeup on scoring strategies, naming Karmada and Borg as the real prior art this is a simplified version of.

## Stage 5: priority and preemption

Building: priority-based preemption, so a high priority Workload can evict lower priority ones when a cluster is full.

Not doing: scale testing. This gets proven correct on a handful of real objects first.

Exit: DESIGN.md section on preemption storms and fairness, what happens when multiple high priority workloads compete for the same room.

## Stage 6: scale simulation

Building: inject thousands of fake Node objects to simulate something like a 100-cluster, 10k-node fleet, and watch how the scheduler behaves under that load.

Not doing: touching real cloud infrastructure. This all runs against kind with synthetic data.

Exit: DESIGN.md section on what breaks first past this scale and what would need to change (sharding the controller, etc). This is the section that matters most, since nobody can rent a real 10k-node fleet solo.

## Stage 7: terraform, polish, and failure demos

Building: terraform module to stand up the local kind clusters (and an optional cloud variant, documented but not left running), a real README, and a recorded demo of failure handling. Killing a node or a cluster mid-placement and watching it recover.

Not doing: anything from meshgate or opsloop. Security, networking, and the gitops/temporal glue live in their own repos, not here.

Exit: repo is public-ready.

## rules that apply to every stage

- Nothing moves to the next stage until the current one's DESIGN.md section is actually written, not just the code.
- fleetd never grows security policy, admission control, or cloud networking logic. That's meshgate's job.
- fleetd never grows terraform-apply automation, gitops, or long running workflows beyond stage 7's basic module. That's opsloop's job.
- If a stage starts pulling in scope from a later stage, stop and split it out instead of letting it grow.
