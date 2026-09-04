# KEP-13502: Unscheduled Pods Timeout

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories (Optional)](#user-stories-optional)
    - [Story 1](#story-1)
  - [Notes/Constraints/Caveats (Optional)](#notesconstraintscaveats-optional)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Kueue Configuration API](#kueue-configuration-api)
  - [Linking Pods to Workloads](#linking-pods-to-workloads)
  - [UnschedulablePodsTracker controller](#unschedulablepodstracker-controller)
  - [Workload PodsScheduled condition](#workload-podsscheduled-condition)
  - [Workload PodsReady condition](#workload-podsready-condition)
  - [Admission cycle and reset](#admission-cycle-and-reset)
  - [Timeout interaction](#timeout-interaction)
  - [Eviction and requeue](#eviction-and-requeue)
  - [MultiKueue, ConcurrentAdmission and elastic jobs](#multikueue-concurrentadmission-and-elastic-jobs)
  - [Version skew and rolling upgrade](#version-skew-and-rolling-upgrade)
  - [Open questions](#open-questions)
  - [Test Plan](#test-plan)
    - [Prerequisite testing updates](#prerequisite-testing-updates)
    - [Unit tests](#unit-tests)
    - [Integration tests](#integration-tests)
    - [e2e tests](#e2e-tests)
  - [Graduation Criteria](#graduation-criteria)
  - [Backward compatibility](#backward-compatibility)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
  - [Per-integration <code>PodsScheduled</code> on <code>GenericJob</code>](#per-integration-podsscheduled-on-genericjob)
  - [Restarting <code>timeout</code> once all required Pods are scheduled](#restarting-timeout-once-all-required-pods-are-scheduled)
  - [Workload controller reading <code>PodsScheduled</code> directly](#workload-controller-reading-podsscheduled-directly)
  - [Resetting <code>PodsScheduled</code> from the admission patch](#resetting-podsscheduled-from-the-admission-patch)
  - [Removing <code>PodsScheduled</code> when the admission ends](#removing-podsscheduled-when-the-admission-ends)
  - [A <code>SchedulingObserved</code> condition](#a-schedulingobserved-condition)
  - [A dedicated feature gate](#a-dedicated-feature-gate)
<!-- /toc -->

## Summary

Add `waitForPodsReady.unschedulableTimeout`, which bounds, within the existing
`waitForPodsReady.timeout` window, how long an admitted Workload may wait for all the Pods
required by its admission to be scheduled (bound to a node) or to have succeeded. The existing
`timeout` is intended for post-schedule startup (image pull, init containers, readiness probes).

A new `UnschedulablePodsTracker` controller reports the scheduling state of the Pods it
observes on a new `PodsScheduled` Workload condition whenever `waitForPodsReady` is enabled.
The job framework surfaces it to the existing `PodsReady`-based timeout machinery as the
`PodsReady` reason `WaitForScheduling`, and the workload controller evicts with the existing
`PodsReadyTimeout` reason and the new `WaitForScheduling` underlying cause.

## Motivation

On on-premises clusters, pods may take a long time to become Ready after scheduling because
of image pulling or initialization. Operators often set `waitForPodsReady.timeout` to 30
minutes to accommodate that.

However, pods that are not yet scheduled due to timing or transient state inconsistencies
between Kueue and the scheduler should be detected and requeued sooner. A separate timeout
allows faster recovery without shortening the time allowed for normal pod startup.

### Goals

- Add a configurable timeout for admitted Workloads whose required Pods stay unscheduled.
- Evict and requeue Workloads that exceed the unschedulable timeout, reusing the existing
  `waitForPodsReady` eviction and requeue machinery with a dedicated `WaitForScheduling`
  underlying cause.
- When `unschedulableTimeout` is set, evict a Workload whose required Pods are still
  unscheduled `unschedulableTimeout` after Kueue first observed them unscheduled in the current
  admission, and never later than `timeout` after the admission. The overall `timeout`
  deadline measured from the admission is unchanged.
- Detect scheduling state centrally so custom in-house job integrations work without
  per-integration code, without extending the `GenericJob` interface and without having the
  job framework observe Pods.
- Start every admission from a clean observation: the readers identify the observations of
  the current admission by `lastTransitionTime > Admitted.lastTransitionTime` and the tracker
  re-stamps that time on the first observation of every admission; `PodsReady` is reset at the
  quota release and the tracker additionally attempts, best-effort, to reset `PodsScheduled`
  when an admission ends.

### Non-Goals

- Replacing or modifying kube-scheduler scheduling queue timeouts.
- Changing the MultiKueue status sync. The tracker does not observe Workloads whose execution
  is delegated to a MultiKueue worker (on the manager cluster); those keep today's `timeout`
  measured from the admission.
- A separate top-level eviction reason or requeue strategy; the existing `PodsReadyTimeout`
  reason and `requeuingStrategy` are reused, with a dedicated underlying cause
  `WaitForScheduling` (parallel to `WaitForStart` and `WaitForRecovery`). The `Evicted`
  condition reason and its message (`Exceeded the PodsReady timeout <namespace/name>`) are
  unchanged.
- Extending the overall deadline: scheduling delays still consume the `timeout` budget.
- Reporting Workloads whose Pods cannot be observed (see [Open questions](#open-questions)).

## Proposal

Extend `WaitForPodsReady` with an optional `unschedulableTimeout` field. A new
`UnschedulablePodsTracker` controller (following the TopologyUngater pattern) lists the Pods
linked to each admitted Workload and reports the `PodsScheduled` Workload condition:
`False` with reason `WaitForScheduling` while at least one required Pod is not scheduled,
`True` with reason `AllRequiredPodsScheduled` once all required Pods are scheduled (bound to a
node) or have succeeded. The tracker is the only writer that decides the value of
`PodsScheduled`.

On the initial startup path, the job framework reconciler translates a `PodsScheduled=False`
/ `WaitForScheduling` condition of the current admission into `PodsReady=False` /
`WaitForScheduling`; the recovery path is unchanged. The workload controller selects the timer
solely from the `PodsReady` reason: `WaitForScheduling` selects the scheduling deadline
(`unschedulableTimeout` since the `PodsScheduled` transition, capped at `timeout` since the
admission), `WaitForStart` selects `timeout` since the admission, and `WaitForRecovery` selects
`recoveryTimeout`.

When an admission ends (the Workload is evicted or its quota reservation is released), the
quota-release helper resets `PodsReady` in the same patch as the release, and the tracker
attempts to reset `PodsScheduled` asynchronously, best-effort. The current admission is
identified by comparing `lastTransitionTime` with `Admitted.lastTransitionTime`, and the next
observation always re-stamps `lastTransitionTime`, so every admission starts from a fresh
observation even when the reset is overtaken.

All of the above is active whenever `waitForPodsReady` is enabled (the `DisableWaitForPodsReady`
feature gate is off and `waitForPodsReady` is configured); there is no new feature gate.
Setting `unschedulableTimeout` only adds the shorter scheduling deadline.

### User Stories (Optional)

#### Story 1

An operator configures Kueue with a shorter scheduling window and a longer startup window:

```yaml
apiVersion: config.kueue.x-k8s.io/v1beta2
kind: Configuration
waitForPodsReady:
  timeout: 30m
  unschedulableTimeout: 5m
```

A Workload is admitted but its Pods remain Pending because of a transient scheduler glitch.
Five minutes after the tracker first observed the unscheduled Pods (and at the latest 30
minutes after the admission), Kueue evicts and requeues the Workload with the underlying
cause `WaitForScheduling`. A Pod that is scheduled but still pulling an image is allowed the
remainder of the 30-minute `timeout` measured from the admission, as today;
`unschedulableTimeout` only shortens the wait while required Pods are unscheduled.

### Notes/Constraints/Caveats (Optional)

- `unschedulableTimeout` does not change kube-scheduler behavior or the MultiKueue status
  sync.
- The scheduling deadline is derived from the `lastTransitionTime` of the
  `PodsScheduled=False` / `WaitForScheduling` condition of the current admission. The tracker
  stamps that time with its own clock on the first `False` / `WaitForScheduling` write of an
  admission, also when only the reason changes (for example from the lifecycle reason written
  when the previous admission ended), and it never writes earlier than one second after the
  `Admitted` transition, so that the timestamp is strictly later than
  `Admitted.lastTransitionTime` at second granularity.
- Within one admission, `PodsScheduled` only moves from `False` to `True`. A Pod lost after
  the Workload became ready is reported on `PodsReady` only (`WaitForRecovery`), and the
  workload controller applies `recoveryTimeout`; `PodsScheduled` is not consulted.
- A current-admission `PodsScheduled` observation is written independently of whether
  `unschedulableTimeout` is set; there is no new feature gate. It starts when the tracker
  observes a live Pod, or when retained Succeeded Pods alone fill the whole admission.
  No current-admission observation is written while neither condition holds, or for
  ConcurrentAdmission Variant Workloads and Workloads delegated to a MultiKueue worker.
  Because the lifecycle reset is best-effort, a condition from a previous admission may
  remain visible; readers ignore it unless it is an observation pair transitioned strictly
  after the current `Admitted` transition.
- While `waitForPodsReady` is enabled, the built-in template-based integrations propagate the
  `kueue.x-k8s.io/workload` and `kueue.x-k8s.io/workload-uid` annotations to the Pod
  templates of the jobs they start, and the pod-based integrations apply them to the gated
  Pods when those Pods are started, so the tracker does not depend on the
  TopologyAwareScheduling feature gate. Pods created before an upgrade always lack
  `workload-uid`, but may already carry `workload` from TopologyAwareScheduling or
  SchedulerLibraryIntegration and are then accepted by name; Pods lacking both the
  `workload-slice-name` and the `workload` indexing annotations are not observed. For a
  Workload whose Pods are all unobserved, the tracker writes no current-admission
  observation and the regular `timeout` measured from the admission applies (see
  [Version skew and rolling upgrade](#version-skew-and-rolling-upgrade)).
- A Pod held by a Kueue scheduling gate (for example the topology gate) is not bound to a
  node and counts as unscheduled.
- The reset of `PodsScheduled` at the end of an admission is asynchronous and best-effort; the
  timers and the `False` to `True` rule do not depend on it (see
  [Admission cycle and reset](#admission-cycle-and-reset)).

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| Evict a healthy Workload on a transient list/index error | Return the error (requeue) and do not patch `PodsScheduled` from failed data. The job framework maps only a `PodsScheduled=False` / `WaitForScheduling` whose `lastTransitionTime` is later than `Admitted.lastTransitionTime` (written by a successful tracker reconcile) to `PodsReady` / `WaitForScheduling`; the workload controller acts on that reason only. |
| Stale `PodsScheduled=False` after a later list/index failure | No `SchedulingObserved` condition (per maintainer feedback); a prior successful observation may remain until the next successful reconcile; accepted trade-off. A stale `False` cannot leak across admissions: the readers ignore any `PodsScheduled` not transitioned strictly after `Admitted`, the first observation of the next admission re-stamps `lastTransitionTime`, and the tracker additionally attempts a best-effort reset when the admission ends. |
| The reset of `PodsScheduled` is overtaken by an immediate re-admission | The timers and the `False` to `True` rule rely on `lastTransitionTime > Admitted.lastTransitionTime` and on the one-second settle of the tracker, not on the reset; the leftover observation is overwritten by the next observation. |
| Increased requeue churn when `unschedulableTimeout` is too low | Operator tuning; existing `requeuingStrategy` backoff. |
| Consumers break on the new `PodsScheduled` condition or the new `PodsReady` reason | Document in backward compatibility; `PodsScheduled` may be written and `PodsReady` may report `WaitForScheduling` whenever `waitForPodsReady` is enabled, even when `unschedulableTimeout` is unset. |
| The tracker and the admission patches both update the Workload status | The tracker is the only writer that decides the value of `PodsScheduled` and applies it with its own field manager on the server-side apply path. Server-side apply admission patches exclude `PodsScheduled` from their payload (it is not an admission-managed condition), and the merge-patch admission patches (`WorkloadRequestUseMergePatch`) carry the whole `status.conditions` list but are strict on `resourceVersion`, so a stale copy cannot overwrite the tracker's value. |
| Workloads whose Pods cannot be observed because every Pod lacks both indexing annotations (for example, a job started before the upgrade without TopologyAwareScheduling or SchedulerLibraryIntegration, or an integration that does not propagate the annotations) silently get the regular `timeout` only | Documented in the user guide and in the version skew section; a Warning event or log is an [open question](#open-questions). |

## Design Details

### Kueue Configuration API

```yaml
waitForPodsReady:
  timeout: 30m
  unschedulableTimeout: 5m
```

```go
type WaitForPodsReady struct {
    // ...
    // UnschedulableTimeout bounds the time an admitted Workload may wait for all
    // the Pods required by its admission to be scheduled (bound to a node) or to
    // have succeeded. The scheduling deadline is measured since the
    // UnschedulablePodsTracker first observed an unscheduled required Pod in the
    // current admission (the lastTransitionTime of the PodsScheduled=False
    // condition with reason WaitForScheduling) and never exceeds timeout since
    // the admission. When the deadline is exceeded, the Workload is evicted with
    // the PodsReadyTimeout reason and the WaitForScheduling underlying cause,
    // and requeued after the backoff delay.
    // Once all the required Pods have been scheduled, the Workload keeps the
    // regular timeout since the admission to become ready; after it has been
    // ready, a lost Pod is governed by recoveryTimeout only.
    // Pods are linked to the Workload by the kueue.x-k8s.io/workload (or
    // workload-slice-name) annotation added to the Pod templates, or to the
    // gated Pods of the pod-based integrations, when the job is started; Pods
    // without it (for example created before an upgrade) are not observed and
    // only the regular timeout applies.
    // Must be positive and must not exceed timeout. When unset, unscheduled Pods
    // are subject to the regular timeout only.
    // +optional
    UnschedulableTimeout *metav1.Duration `json:"unschedulableTimeout,omitempty"`
}
```

**Validation:**

- `unschedulableTimeout` must be positive; `0s` and negative values are rejected.
- `unschedulableTimeout` must not exceed `timeout`. The configuration is validated after
  defaulting ([`apis/config/v1beta2/defaults.go`](../../apis/config/v1beta2/defaults.go) replaces
  a zero `WaitForPodsReady.Timeout` with `DefaultWaitForPodsReadyTimeout` before validation),
  so the comparison uses the effective `timeout`.
- Validation cases: omitted `unschedulableTimeout` (no scheduling deadline), `0s` (rejected),
  negative (rejected), equal to the effective `timeout` (accepted), greater than the effective
  `timeout` (rejected).
- Setting `unschedulableTimeout` has no effect while the `DisableWaitForPodsReady` feature
  gate is on.

### Linking Pods to Workloads

The tracker needs to list the Pods of a Workload without knowing the job type. Two Pod
annotations serve that purpose, both applied through `podset.Merge` when a job is started:
the built-in template-based integrations propagate them to the Pod templates of the job
(and remove them when the job is stopped), and the pod-based integrations (pod groups,
Deployments, StatefulSets and LeaderWorkerSets) apply them to the gated Pods when those Pods
are started:

- `kueue.x-k8s.io/workload` ([`WorkloadAnnotation`](../../apis/kueue/v1beta2/topology_types.go)):
  the name of the admitted Workload. It is added today when the TopologyAwareScheduling or
  the SchedulerLibraryIntegration feature gate is enabled; it is now also added whenever
  `waitForPodsReady` is enabled. Elastic jobs additionally carry
  `kueue.x-k8s.io/workload-slice-name`.
- `kueue.x-k8s.io/workload-uid` (`WorkloadUIDAnnotation`, new): the UID of the admitted
  Workload (for elastic jobs, of the slice admitted when the Pod was started). It is added
  only while `waitForPodsReady` is enabled. The tracker ignores a Pod whose UID annotation
  differs from the UID of the Workload it evaluates, so that the Pods of a deleted Workload
  are not attributed to a re-created Workload with the same name. The annotation therefore
  protects non-elastic, non-pod-group Workloads only: the Pods of an elastic job (whose
  `workload-slice-name` annotation matches the slice under evaluation) and the Pods of a pod
  group (whose Workload carries the `kueue.x-k8s.io/is-group-workload` annotation) are matched
  by name regardless of the UID, and Pods without the annotation are accepted by name.
  `podset.Merge` may always overwrite a stale value of this annotation on a Pod template
  (it changes whenever the Workload or the admitted slice is re-created) instead of reporting
  a `BadPodSetsUpdateError`.

The Pods are listed through the existing Pod field index on the `workload-slice-name` /
`workload` annotation (`WorkloadSliceNameKey` in `pkg/controller/core/indexer`), which is
registered whenever `waitForPodsReady` is enabled. A Pod that carries neither annotation is
not indexed and is never observed. For elastic jobs the Pods of the whole slice chain are
counted against the grant of the active (admitted) slice.

### UnschedulablePodsTracker controller

A new controller, `UnschedulablePodsTracker`, follows the same architectural pattern as
`TopologyUngater` ([KEP-2724](../2724-topology-aware-scheduling/README.md)) and
`ElasticJobUngater`:

- Reconciles **Workload** resources (not per Job integration) and watches **Pod**
  create/update/delete events, batching reconcile requests (`UpdatesBatchPeriod`). A Pod
  event is relevant when the binding, the phase, the `workload`, `workload-slice-name` or
  `workload-uid` annotation, the `kueue.x-k8s.io/podset` label, or the presence of a deletion
  timestamp changes. A Workload event is relevant when the Workload is tracked or carries a
  `PodsScheduled` condition.
- Tracks the Workloads that are admitted, hold their quota reservation, and are neither
  finished nor evicted. ConcurrentAdmission Variant Workloads and Workloads delegated to a
  MultiKueue worker are never tracked.
- Runs only on the leader.

**Reconcile flow:**

1. If the Workload is an elastic slice, resolve the active (admitted) slice of the chain and
   evaluate it instead, also while both the replaced and the replacement slice are still
   admitted.
2. If the Workload is finished, keep its last observation and stop.
3. If the Workload is delegated to a MultiKueue worker, remove any existing `PodsScheduled`
   condition (whoever wrote it) and stop.
4. If the Workload is not tracked (evicted, quota released, not admitted, or a Variant),
   write the lifecycle reset when it is due (see
   [Admission cycle and reset](#admission-cycle-and-reset)) and stop.
5. Do not write anything before `Admitted.lastTransitionTime + 1s` (settle).
6. If the current admission already has `PodsScheduled=True`, stop without listing Pods (the
   condition is sticky for the rest of the admission).
7. List the Pods linked to the Workload, ignoring those whose `workload-uid` annotation
   does not match (see above). On a list or index error, return the error (requeue) and do
   **not** patch `PodsScheduled` from failed data.
8. Zero-Pod rule: if no Pod is linked and the current admission has no observation, write
   nothing (also for Workloads whose PodSets have count zero).
9. Summarize the Pods per admitted PodSet (formula below). If the current admission has no
   observation and no live Pod is left (only terminal or terminating Pods), write `True` only
   when the retained succeeded Pods alone, capped per PodSet at the grant, fill the whole
   admission; otherwise write nothing. Terminal Pods alone never open the scheduling window,
   and failed or reclaimable slots alone never complete it.
10. If the current admission has `PodsScheduled=False` and not all required Pods are
    scheduled yet, write nothing (the `lastTransitionTime` is preserved).
11. Otherwise write the observation: `False` / `WaitForScheduling` or `True` /
    `AllRequiredPodsScheduled`, with `lastTransitionTime` set to the current time and
    `observedGeneration` set to the Workload generation.

**Counting formula:** for each admitted PodSet,

```
scheduled = min(granted, activeScheduled + max(succeededRetained, reclaimable))
required  = sum(granted)
```

where `granted` is `.status.admission.podSetAssignments[*].count`, `activeScheduled` counts
the Pods that are neither `Succeeded` nor `Failed`, have no deletion timestamp, and are bound
to a node (`spec.nodeName` set or `PodScheduled=True`), `succeededRetained` counts the Pods in
phase `Succeeded` that are still present (also when being deleted), and `reclaimable` is
`.status.reclaimablePods[podSet]` when the `ReclaimablePods` feature gate is enabled (covers
succeeded Pods already deleted). Failed Pods and Pods being deleted never satisfy a slot.
Surplus Pods are capped at the grant. All required Pods are scheduled when
`scheduled == required`.

**Writes:** the tracker writes when the status or the reason changes, when the current
admission has no observation yet (re-stamping `lastTransitionTime` even when the previous
admission ended with the same status and reason), and for a lifecycle reset whose target
differs from the existing condition. The messages are fixed
(`At least one required pod is not scheduled` / `All required pods were scheduled or
succeeded`); `observedGeneration` is set on every write but a change of the generation alone
does not trigger a write.

**Patch path:** observations and resets use a strict server-side apply with the tracker's
own field manager (`kueue-unschedulable-pods-tracker`), or a strict JSON merge patch when the
`WorkloadRequestUseMergePatch` feature gate is on (the condition then carries no apply owner of
the tracker; the API server may record an Update manager). A patch from a stale copy gets a
conflict and the Workload is requeued. The removal on a MultiKueue manager uses a strict merge
patch, which does not depend on the field ownership of the condition.

**Required-pod semantics:**

| Case | Behavior |
|------|----------|
| Pod not yet created (count below granted) | Not all scheduled; `PodsScheduled=False` / `WaitForScheduling` once a live Pod is observed. With no Pod at all and no observation of the current admission, nothing is written. |
| Pod held by a Kueue scheduling gate | Unscheduled. |
| Pod with a deletion timestamp (not `Succeeded`) | Does not satisfy a slot and does not open the scheduling window. |
| Terminated Pod (`Succeeded`), retained or reflected in `reclaimablePods` | Counts as scheduled; no replacement required. Retained succeeded Pods filling the whole admission produce `True` even without a live Pod. |
| Terminated Pod (`Failed`) | Does not satisfy a slot; a replacement Pod is required. Alone it never opens the scheduling window. |
| Pod deleted or preempted while the Workload was running | `PodsScheduled` stays `True` (sticky); `PodsReady=False` / `WaitForRecovery` and `recoveryTimeout` govern the eviction. |
| Surplus Pods (for example after a scale-down) | Capped at the grant. |
| Optional PodSets with zero count | No Pods required; the check passes for that PodSet. |
| List/index client error | No patch from failed data; requeue. |
| Successful observation, required Pods unscheduled | `PodsScheduled=False` / `WaitForScheduling`; surfaced as `PodsReady` / `WaitForScheduling` by the job framework; eviction per the timeout table. |
| Successful observation, all required Pods scheduled or succeeded | `PodsScheduled=True` / `AllRequiredPodsScheduled`; hand off to the job framework for `PodsReady`. |
| Workload finished | Last observation retained. |
| Workload evicted or quota reservation released | Best-effort reset to `False` with the lifecycle reason (see below). |
| ConcurrentAdmission Variant, MultiKueue manager-side Workload | Not tracked; an existing condition is removed on the MultiKueue manager. |
| Elastic slice chain | Counted against the active slice; a replaced (finished) slice keeps its last observation. |

### Workload PodsScheduled condition

Add constants:

```go
WorkloadPodsScheduled = "PodsScheduled"
WorkloadWaitForScheduling = "WaitForScheduling"
WorkloadAllRequiredPodsScheduled = "AllRequiredPodsScheduled"
```

`WaitForScheduling` is also a `PodsReady` reason and an eviction underlying cause.

The `UnschedulablePodsTracker` is the only writer that decides the value of `PodsScheduled`,
both for the observations and for the lifecycle reset. The admission patches never change its
value (see [Risks and Mitigations](#risks-and-mitigations)). Only one `PodsScheduled` entry
exists at a time (`SetStatusCondition` replaces by `type`).

`PodsScheduled=True` is a history condition: it stands for the rest of the admission even if
a Pod is later deleted or fails, and its `observedGeneration` may lag behind the Workload
generation; readers must not require them to match.

`PodsScheduled=False` (scheduling in progress):

```yaml
- type: PodsScheduled
  status: "False"
  reason: WaitForScheduling
  message: "At least one required pod is not scheduled"
```

`PodsScheduled=True` (all required pods scheduled or succeeded):

```yaml
- type: PodsScheduled
  status: "True"
  reason: AllRequiredPodsScheduled
  message: "All required pods were scheduled or succeeded"
```

`PodsScheduled=False` after the admission ended (here by the scheduling timeout):

```yaml
- type: PodsScheduled
  status: "False"
  reason: PodsReadyTimeout
  message: "Exceeded the PodsReady timeout default/my-workload"
```

### Workload PodsReady condition

The `PodsReady=False` reasons are `WaitForStart` (the Pods have not been ready since the
admission and no required Pod is known to be unscheduled, or the Workload is not admitted),
`WaitForScheduling` (new: the Pods have not been ready since the admission and the
`PodsScheduled` condition of the current admission reports unscheduled Pods) and
`WaitForRecovery`. The `PodsReady=True` reasons `Started` and `Recovered` are unchanged.

The job framework reconciler ([`generatePodsReadyCondition`](../../pkg/controller/jobframework/reconciler.go))
keeps evaluating `GenericJob.PodsReady(ctx, client)`. It does **not** call per-integration
scheduling probes and does **not** observe Pods; it only translates the tracker's observation
into a `PodsReady` reason. It does **not** select eviction underlying causes (the workload
controller owns timeout and cause selection, as for `WaitForStart` / `WaitForRecovery` today).

**Recovery predicate** (when recovery begins), aligned with `generatePodsReadyCondition`:

- **Recovery path:** `PodsReady.Reason == WaitForRecovery`, **or** `PodsReady.Status` was
  `True` and `PodsReady()` is now false.
- **Initial scheduling path:** the Workload has never reached `PodsReady=True` in the current
  admission (the `PodsReady` condition is nil, or its reason is `WaitForStart`,
  `WaitForScheduling` or the legacy `PodsReady`).

1. **Initial scheduling path:** when `PodsReady()` is true, `PodsReady=True` / `Started` as
   today. Otherwise, if the current admission has `PodsScheduled=False` /
   `WaitForScheduling` (transitioned strictly after `Admitted.lastTransitionTime`) and the
   Workload is not delegated to a MultiKueue worker, write `PodsReady=False` /
   `WaitForScheduling` (message `Not all pods are ready or succeeded`); otherwise write
   `WaitForStart` as today. A Pod failure before the first `PodsReady=True` stays on this
   path. A failed MultiKueue delegation lookup is returned as a reconcile error so that the
   reconcile is retried instead of acting on a guess.
2. **Recovery path:** evaluate `PodsReady()` and set or retain `WaitForRecovery` per
   `generatePodsReadyCondition`, without reading `PodsScheduled`; the eviction uses
   `recoveryTimeout`.

**Reset at quota release:** `UnsetQuotaReservationWithCondition` (the helper used by every
quota release path) resets `PodsReady` to `False` / `WaitForStart` with the canonical message,
only when `.status.admission` was set and the Workload was `Admitted=True` before the patch,
the `DisableWaitForPodsReady` feature gate is off, the Workload is not a ConcurrentAdmission
Variant, and a `PodsReady` condition exists and is not already `False` / `WaitForStart` (a
message-only difference is not rewritten, as the job framework does). A `True` to `False`
reset is stamped with the current time; when an existing `False` condition (`WaitForRecovery`
or `WaitForScheduling`) changes only its reason, normal condition semantics preserve its
`lastTransitionTime`. Nothing is written when the condition is absent (see
[Open questions](#open-questions)). The reset travels in the same patch as the quota release:
`PodsReady` is an admission-managed condition and is carried by both the server-side apply
and the merge-patch admission paths. Without it, a `PodsReady=True` or `WaitForRecovery` left
over from the previous admission would make the job framework treat the next admission as a
recovery.

### Admission cycle and reset

The observation interval of an admission starts at the `Admitted=True` transition (plus the
one-second settle) and ends when the Workload is evicted (`Evicted=True`, while `Admitted` and
the quota reservation are still `True`) or when its quota reservation is released, whichever
comes first. At that point the tracker attempts, asynchronously and best-effort, to reset
`PodsScheduled` to `False` with the lifecycle reason and message of the ending admission:

- the reason and message of the `Evicted=True` condition when present (for example
  `PodsReadyTimeout`, `Preempted`, `FlavorMigration`, `AdmissionCheck`, `ClusterQueueStopped`,
  `LocalQueueStopped`, `NodeFailures`, `Deactivated` or `DeactivatedDueTo<Cause>`); the
  scheduler resets `Evicted` at every admission, so a `True` `Evicted` always belongs to the
  ending admission;
- otherwise the reason and message of the `QuotaReserved=False` condition (for example
  `OnHold`, `AdmissionGated`, `Inadmissible`, `Pending` or `Waiting`); an empty message is
  replaced by `Quota reservation released`.

None of these reasons collides with the two observation reasons. A `True` to `False` reset is
stamped with the current time; a reason-only change (for example the deactivation of an
already evicted Workload rewriting the `Evicted` reason to `DeactivatedDueTo<Cause>`, which
produces a second reset) keeps the existing `lastTransitionTime`. A finished Workload keeps its
last observation.

The reset is **asynchronous and best-effort**. The tracker and the job framework are
independent controllers: for integrations whose job is already inactive when it is evicted
(pod groups, RayCluster, RayService, SparkApplication) the eviction and the quota release can
happen in a single job framework reconcile, and with `backoffBaseSeconds: 0` the re-admission
can precede the tracker's reset (its strict patch conflicts and is retried). The observation of
the previous admission then remains until the next observation overwrites it (indefinitely if
the new admission never produces a Pod). This is harmless because the timers and the `False`
to `True` rule never rely on the reset:

- The readers (the job framework and the workload controller) accept a `PodsScheduled`
  condition as an observation of the current admission only when its status and reason are
  one of the two observation pairs and its `lastTransitionTime` is strictly later than
  `Admitted.lastTransitionTime`; lifecycle reasons are ignored regardless of the time.
- The tracker never writes before `Admitted.lastTransitionTime + 1s`, so every observation of
  the new admission has a strictly later `lastTransitionTime` at second granularity, and the
  first observation of an admission always re-stamps the time.

The reset exists for observability: when it succeeds, it records how the admission ended. If
it is overtaken and the next admission produces no Pod, the previous condition may remain
indefinitely, but readers ignore it and the first new observation always re-stamps
`lastTransitionTime`.

Within one admission, `PodsScheduled` only transitions from `False` to `True`; once `True`,
the tracker stops listing the Pods of that admission.

### Timeout interaction

The workload controller selects the deadline from the `PodsReady` reason. It reads
`PodsScheduled` only to find the start of the scheduling deadline, after the `PodsReady`
reason selected it.

| `PodsReady` | `unschedulableTimeout` | Deadline | Underlying cause |
|-------------|------------------------|----------|------------------|
| `True` | any | none | — |
| nil, `WaitForStart`, legacy `PodsReady` | any | `Admitted.lastTransitionTime + timeout` (unchanged) | `WaitForStart` |
| `WaitForScheduling` | unset | `Admitted.lastTransitionTime + timeout` | `WaitForScheduling` |
| `WaitForScheduling` | set | `min(cur.lastTransitionTime + unschedulableTimeout, Admitted.lastTransitionTime + timeout)`, where `cur` is the `PodsScheduled=False` / `WaitForScheduling` of the current admission; without one, `Admitted.lastTransitionTime + timeout` | `WaitForScheduling` |
| `WaitForRecovery` | any | `PodsReady.lastTransitionTime + recoveryTimeout` (unchanged; no deadline when `recoveryTimeout` is unset) | `WaitForRecovery` |

`PodsScheduled.lastTransitionTime` is stamped by the tracker at write time (after the
one-second settle following `Admitted`), so the scheduling deadline starts when Kueue first
observes the unscheduled Pods, not at the admission and not at the Pod's own transition. The
reader accepts only a `PodsScheduled` transitioned strictly after
`Admitted.lastTransitionTime`; when the `PodsReady` reason is `WaitForScheduling` but no such
condition exists (a stale `PodsReady`), the deadline falls back to `timeout` from the
admission.

A Workload delegated to a MultiKueue worker (the `MultiKueue` feature gate is on and the
Workload has a MultiKueue admission check) is never subject to the scheduling deadline: a
`WaitForScheduling` reason is treated as `WaitForStart` (deadline and cause). The lookup is a
cached read that runs only when the regular evaluation selected `WaitForScheduling`; a lookup
error is returned as a reconcile error.

When `unschedulableTimeout` is not configured, `PodsScheduled` and the `WaitForScheduling`
reason are still written (while `waitForPodsReady` is enabled) but every deadline equals
today's `timeout` measured from the admission.

### Eviction and requeue

Eviction uses `WorkloadEvictedByPodsReadyTimeout`. The **workload controller** owns timeout
evaluation and underlying-cause selection in `admittedNotReadyWorkload`
([`pkg/controller/core/workload_controller.go`](../../pkg/controller/core/workload_controller.go)),
keyed on the `PodsReady` reason:

1. `PodsReady=False` / `WaitForRecovery` → `recoveryTimeout` from
   `PodsReady.lastTransitionTime`; underlying cause `WaitForRecovery`.
2. `PodsReady=False` / `WaitForScheduling` → the scheduling deadline of the table above;
   underlying cause **`WaitForScheduling`** (unless the Workload is delegated to a MultiKueue
   worker, in which case the `WaitForStart` row applies).
3. `PodsReady=False` / `WaitForStart`, nil or legacy `PodsReady` → `timeout` from the
   admission; underlying cause `WaitForStart`.

The `Evicted` reason stays `PodsReadyTimeout` and its message
(`Exceeded the PodsReady timeout <namespace/name>`) is unchanged; only the underlying cause
reported in the event, in `.status.schedulingStats.evictions` and in the `underlying_cause`
metric label gains the `WaitForScheduling` value. Pending Pods are evicted as soon as the
deadline passes; the controller does not wait for a further Pod event. Requeue backoff uses the
existing `requeuingStrategy`.

The `WaitForScheduling` value is added to the `underlying_cause` label of
`kueue_evicted_workloads_total`, `kueue_local_queue_evicted_workloads_total` and
`kueue_evicted_workloads_once_total`, documented as: "means that the workload was evicted by
the PodsReady timeout while its PodsReady condition reported WaitForScheduling (at least one
required Pod was observed unscheduled)". `kueue_pods_ready_to_evicted_time_seconds` is
unchanged: it is recorded only when `PodsReady=True` at eviction time, which never holds for a
scheduling timeout.

### MultiKueue, ConcurrentAdmission and elastic jobs

- **MultiKueue (manager cluster):** the Pods of a delegated Workload run on a worker cluster,
  so nothing can be observed locally. The tracker removes any `PodsScheduled` condition of a
  delegated Workload (whoever wrote it, through an ownership-independent merge patch). On the
  initial not-ready path, the job framework does not propagate `WaitForScheduling` to
  `PodsReady` and uses `WaitForStart`; normal ready and recovery-path behavior is unchanged.
  The workload controller does not apply the scheduling deadline. These three guards matter
  because the admission checks of a ClusterQueue are also synced to already admitted
  Workloads: a Workload observed locally can become delegated in the middle of an admission,
  and the workload controller may run before the job framework rewrites the `PodsReady`
  reason. The tracker performs the cached admission check lookup
  (`admissioncheck.ShouldSkipLocalExecution`) for every non-finished Workload before deciding
  whether to observe or lifecycle-reset it. The job framework and workload controller perform
  the lookup only after a current `WaitForScheduling` decision exists. All three propagate
  lookup errors. Finished Workloads keep their last observation.
- **ConcurrentAdmission Variants:** skipped by the tracker (observation and reset) and by
  the `PodsReady` reset; Variants receive `PodsReady` copied from their Parent. The Parent is
  tracked.
- **Elastic jobs (`ElasticJobsViaWorkloadSlices`):** a request for a replaced slice is
  redirected to the active slice of the chain, and the Pods of the whole chain are counted
  against the grant of the active slice. A replaced slice, which becomes finished, keeps its
  last observation; the lifecycle reset is written only on the slice that was evicted or
  lost its quota.

### Version skew and rolling upgrade

- Pods created by an older Kueue (before the upgrade) always lack the
  `kueue.x-k8s.io/workload-uid` annotation, but may already carry `kueue.x-k8s.io/workload`
  when TopologyAwareScheduling or SchedulerLibraryIntegration was enabled; such Pods are
  accepted by name and observed. Pods lacking both the `workload-slice-name` and the
  `workload` indexing annotations are not indexed at all: for a Workload whose Pods are all
  unobserved the tracker observes no Pods, the zero-Pod rule writes no current-admission
  observation, and the regular `timeout` measured from the admission applies. Jobs started
  after the upgrade get both annotations and are tracked.
- The one-second settle of the tracker and the `lastTransitionTime > Admitted.lastTransitionTime`
  check make any `PodsScheduled` left from a previous admission harmless, even when the reset
  never happened (older controller, lost patch, or immediate re-admission).
- An older workload controller does not reset `PodsReady` when it releases the quota
  reservation. During a leader handover between versions a `PodsReady=True` or
  `WaitForRecovery` from the previous admission can therefore survive into the next admission
  and be treated as a recovery by the job framework, as it is today. Once the new version
  releases the quota, the reset applies.
- The reset of `PodsScheduled` is asynchronous; a rolling restart may leave the condition
  with a previous observation until the tracker of the new leader catches up. Consumers
  must apply the same `lastTransitionTime` rule as the readers.
- On a downgrade, an older version ignores `PodsScheduled` and the `WaitForScheduling`
  reason; a leftover `PodsScheduled` condition stays on the Workload until it is deleted. The
  older version neither sets nor checks the `workload-uid` annotation, so a leftover value on
  a Pod template is not a conflict for its `podset.Merge` and is removed when the job is
  stopped.

### Open questions

1. **Warning event or log for Workloads whose Pods cannot be observed.** The tracker only
   lists Pods carrying the `workload-slice-name` or `workload` indexing annotation and cannot
   tell a Workload whose Pods all lack both annotations (for example, a job started before the
   upgrade without TopologyAwareScheduling or SchedulerLibraryIntegration, or an integration
   that does not propagate the annotations) from a Workload whose Pods have not been created
   yet. Emitting a Warning event (for example `PodsNotObserved`) or a `V(2)`
   log for an admitted Workload with no observed Pod after the settle requires a
   process-local deduplication (cleanup on Workload deletion, a size bound, re-notification
   after a leader change or restart) and would also fire in the normal case of Pods created
   late. The current design only documents the behavior.
2. **Best-effort reset and early reset at `Evicted=True`.** The tracker resets
   `PodsScheduled` both at `Evicted=True` (before the quota is released, while `Admitted` and
   the quota reservation are still `True`) and at the quota release, asynchronously and
   without a time bound, so an immediate re-admission can overtake it. The alternative is to
   reset at the quota release only, which widens the window during which a `True` of the
   ended admission is visible. The timers and the `False` to `True` rule do not depend on
   either choice.
3. **`PodsReady` reset only when the condition exists.** The quota-release helper rewrites an
   existing `PodsReady` condition and never inserts one, to avoid creating the condition on
   Workloads the job framework never wrote it on (feature disabled, or Workloads not managed
   by the job framework). The alternative is to insert `False` / `WaitForStart` when absent.

### Test Plan

[x] I/we understand the owners of the involved components may require updates to
existing tests to make this code solid enough prior to committing the changes necessary
to implement this enhancement.

#### Prerequisite testing updates

Existing integration coverage for `waitForPodsReady` in the job controller integration tests
and the workload controller unit tests in `pkg/controller/core/workload_controller_test.go`
provide the foundation for this enhancement. The expectations of the existing unit tests do
not change; the integration tests that release quota through `util.SetQuotaReservation(nil)`
already expect `PodsReady=False` / `WaitForStart` regardless of the timestamp.

#### Unit tests

- Configuration (`pkg/config`): `unschedulableTimeout` of `0s`, negative and greater than
  `timeout` are rejected; equal to `timeout` and positive values are accepted; without
  defaulting, `timeout: 0s` with a positive `unschedulableTimeout` reports both the missing
  `timeout` and the exceeded bound. The v1beta2 to v1beta1 conversion drops the field.
- `pkg/workload`: `UnsetQuotaReservationWithCondition` resets `PodsReady` to `False` /
  `WaitForStart` only when the Workload was admitted, the `DisableWaitForPodsReady` gate is
  off, the Workload is not a Variant and a `PodsReady` condition exists (`True` / `Started`,
  `WaitForRecovery`, `WaitForScheduling`); a `WaitForStart` with a different message is not
  rewritten; both patch paths. `CurrentPodsScheduledCondition` accepts only the two
  observation pairs transitioned strictly after `Admitted`, regardless of
  `observedGeneration`.
- `pkg/podset`: a stale `workload-uid` annotation on the template is overwritten without an
  error, with and without the `ElasticJobsViaWorkloadSlices` gate.
- `pkg/util/pod`: `IsScheduled` (`spec.nodeName` or `PodScheduled=True`).
- Job framework: the initial path maps a current-admission `PodsScheduled=False` /
  `WaitForScheduling` to `PodsReady` / `WaitForScheduling`; `True`, reset values and
  observations of a previous admission map to `WaitForStart`; a ready job moves from
  `WaitForScheduling` to `Started`; the recovery path does not read `PodsScheduled`; a
  not-ready Workload on the initial path with a MultiKueue admission check keeps
  `WaitForStart`, and a delegation lookup error is returned without changing `PodsReady`. The quota release after an eviction resets
  `PodsReady=True` / `Started` to `False` / `WaitForStart` and a later readiness records
  `Started` (and the ready-wait metric) again. The `workload` and `workload-uid` annotations
  are injected per the gate combinations.
- Workload controller: timer selection by the `PodsReady` reason for every row of the
  timeout table, the scheduling deadline and its cap, the fallback without a current
  observation, a `PodsScheduled` with `lastTransitionTime <= Admitted.lastTransitionTime`
  ignored, extreme durations; eviction with cause `WaitForScheduling` after the deadline; a
  Workload with a MultiKueue admission check is not evicted before `timeout` from the
  admission and reports `WaitForStart`; a lookup error is returned; no admission check
  lookup happens when `waitForPodsReady` is disabled, the Workload is not admitted, or
  `PodsReady` is `True` or `WaitForStart` (a failing lookup client does not fail the
  reconcile).
- `UnschedulablePodsTracker`: the counting formula (succeeded retained, `reclaimablePods`,
  failed and deleting excluded, surplus capped, Kueue-gated Pods unscheduled); sticky
  `False` to `True`; `lastTransitionTime` re-stamped on the first observation of an
  admission, including a reason-only change from a lifecycle reason and the same pair as
  the previous admission; no write while still unscheduled or when a Pod disappears; no
  generation-only writes; the one-second settle; the zero-Pod boundaries (no Pod with a
  zero grant, deleting Pods only, failed Pods only, succeeded below the grant, succeeded
  filling the grant, `reclaimablePods` filling the grant with and without a current
  observation); the lifecycle reset at `Evicted=True` while the quota is held, after the
  release, on consecutive releases with different reasons or messages, on a deactivation
  rewriting the reason, with the empty-message fallback, and never on finished Workloads,
  Variants or Workloads without the condition; a reset overtaken by a re-admission (conflict,
  requeue, no reset, a new observation after the settle); the UID filter (mismatch ignored,
  missing annotation accepted, elastic slice matched by name, a non-elastic Workload with a
  stale slice annotation ignored, pod groups matched by name); the Pod and Workload event
  predicates; the MultiKueue removal (any existing condition, regardless of ownership and
  target, before the lifecycle reset and after the finished check); elastic redirection to
  the active slice, also while both slices are admitted; list errors write nothing.

#### Integration tests

A dedicated file, `test/integration/singlecluster/controller/jobs/job/unschedulable_pods_timeout_test.go`,
in the job controller suite (the tracker runs in that suite whenever `waitForPodsReady` is
configured):

- Unscheduled Pod: the Job template gets the `workload` and `workload-uid` annotations,
  `PodsScheduled=False` / `WaitForScheduling`, `PodsReady=False` / `WaitForScheduling`,
  eviction with `PodsReadyTimeout` and the `WaitForScheduling` underlying cause (condition,
  `schedulingStats` and metric), then `PodsScheduled=False` / `PodsReadyTimeout` with the
  eviction message and `PodsReady=False` / `WaitForStart` after the release; the
  re-admission produces a fresh `WaitForScheduling` timestamp and a second eviction.
- Field ownership: a strict admission patch of another admission-managed condition leaves
  every field of `PodsScheduled` untouched and the tracker as its only apply owner; a loose
  admission patch from a stale copy leaves it untouched; a strict one conflicts. With
  `WorkloadRequestUseMergePatch` on, an admission patch from a stale copy without retry
  returns a conflict; with `WithRetryOnConflict` (the production path) it refetches and
  succeeds; in both cases every field of `PodsScheduled=True` remains unchanged.
- Scheduled Pod: `PodsScheduled=True`, `PodsReady=False` / `WaitForStart`, no eviction past
  `unschedulableTimeout`; the condition stays `True` after the Pod is deleted.
- Recovery: a running Workload that loses a Pod gets `PodsReady=False` / `WaitForRecovery`
  while `PodsScheduled` stays `True`; no eviction by the scheduling deadline.
- No Pod: no `PodsScheduled` condition, `PodsReady=False` / `WaitForStart`, no eviction
  before `timeout`.
- `unschedulableTimeout` unset: `PodsScheduled` and `PodsReady` / `WaitForScheduling` are
  still written and the eviction happens at `timeout` with cause `WaitForScheduling`.
- `unschedulableTimeout` equal to `timeout`: eviction at `timeout` from the admission, not
  earlier.
- Immediate re-admission (`backoffBaseSeconds: 0`): the new admission gets a new observation
  with a later `lastTransitionTime` and a deadline measured from it; the reset value of the
  ended admission may or may not be observable.
- Workload re-created with the same name: the template UID annotation is updated and the
  job restarts.
- MultiKueue admission check synced to an admitted Workload: the tracker removes
  `PodsScheduled` (including one written under `WorkloadRequestUseMergePatch` before
  switching to server-side apply), the job framework rewrites `PodsReady` to `WaitForStart`,
  and the Workload is not evicted at the original scheduling deadline.
- Regression: the existing `waitForPodsReady` specs (job, `scheduler/podsready`, TAS and
  elastic jobs) pass with the tracker running.
- One representative operator integration (MPIJob) for Pod discovery: the launcher and
  worker templates get the annotations and the `podset` label, `PodsScheduled` moves from
  `False` to `True` as the Pods get scheduled.

#### e2e tests

Extend the `waitforpodsready` e2e coverage in
`test/e2e/sequential/baseline/waitforpodsready_test.go`: a Job with an unsatisfiable node
selector reports `PodsScheduled=False` / `WaitForScheduling` and `PodsReady=False` /
`WaitForScheduling`, is evicted with the `WaitForScheduling` underlying cause no earlier than
`unschedulableTimeout` after the first observation and before `timeout`, increments
`kueue_evicted_workloads_once_total{underlying_cause="WaitForScheduling"}`, and ends with
`PodsReady=False` / `WaitForStart` and `PodsScheduled=False` / `PodsReadyTimeout`.

### Graduation Criteria

- **Beta** in Kueue **v0.20** (see `kep.yaml`); the feature goes directly to beta, as
  [KEP-1282](../1282-pods-ready-requeue-strategy/README.md) did.
- No new feature gate. `PodsScheduled`, the `WaitForScheduling` reason, the Pod annotations
  and the resets are active whenever the existing `DisableWaitForPodsReady` gate is off and
  `waitForPodsReady` is configured; the shortened deadline additionally requires
  `unschedulableTimeout` to be set.
- **Stable** when the feature is implemented, tested, documented in the user guide, and has
  run in at least one release without critical issues.

### Backward compatibility

- `unschedulableTimeout` unset: **eviction timing and duration** are identical to the current
  behavior (`timeout` measured from the admission).
- Whenever `waitForPodsReady` is enabled, the tracker writes a current-admission
  `PodsScheduled` observation after it observes a live Pod, or when retained Succeeded Pods
  alone fill the whole admission. It writes no current-admission observation for the
  documented exclusions. A lifecycle value or observation from a previous admission may
  remain visible when the best-effort reset is overtaken; consumers must apply the
  `lastTransitionTime > Admitted.lastTransitionTime` rule. `PodsReady=False` may carry the
  new reason `WaitForScheduling` on the initial path. The Pod templates of the template-based
  integrations and the gated Pods of the pod-based integrations get the
  `kueue.x-k8s.io/workload` and `kueue.x-k8s.io/workload-uid` annotations. Consumers
  inspecting Workload conditions, Pod templates or Pods must tolerate them.
- `blockAdmission`, `recoveryTimeout` and deactivation are unchanged. `DisableWaitForPodsReady`
  disables the tracker, the annotations, the resets and the new reason; the `workload`
  annotation added for TopologyAwareScheduling or the scheduler library integration is
  unaffected.

## Implementation History

- 2026-07-27: Initial KEP for issue #13502.
- 2026-09: Design revised: timers driven by the `PodsReady` reason, scheduling deadline
  measured from the tracker's first observation and capped at `timeout` from the admission,
  `PodsScheduled` moving only from `False` to `True` within an admission, tracker-owned
  lifecycle reset, `PodsReady` reset at the quota release, `kueue.x-k8s.io/workload-uid`
  annotation, enablement tied to `waitForPodsReady`.

## Drawbacks

- Adds a cluster-scoped controller with pod watches (additional operational surface).
- Central tracking couples scheduling detection with the job framework's readiness path: the
  job framework translates `PodsScheduled` into a `PodsReady` reason (it does not observe
  Pods).
- `PodsScheduled` / `WaitForScheduling` is visible in the Workload status, and two
  annotations are added to the Pod templates (or, for the pod-based integrations, to the
  gated Pods) of every job started under `waitForPodsReady`, even when the short timeout is
  not configured.
- The reset of `PodsScheduled` is best-effort; an observation of the previous admission may
  be visible for a while after an immediate re-admission.

## Alternatives

### Per-integration `PodsScheduled` on `GenericJob`

Each job integration (Job, Pod, JobSet, MPIJob, Kubeflow jobs, AppWrapper, Spark,
RayJob, RayCluster, RayService, TrainJob) implements
`PodsScheduled(ctx, client) (bool, error)` on the `GenericJob` interface.
`generatePodsReadyCondition` calls it to detect scheduling state.

**Reasons for discarding as primary approach**

- High maintenance cost across many integrations; every new job type must implement
  scheduling probes.
- Does not work out-of-the-box for custom in-house integrations that use the job
  framework without upstream changes.
- Duplicates pod-listing logic already centralized for TopologyUngater and
  ElasticJobUngater.

This approach may be reconsidered only if the central controller proves too invasive
during implementation.

### Restarting `timeout` once all required Pods are scheduled

Measure `timeout` from the `PodsScheduled=True` transition, so that scheduling delays do not
consume the startup budget. Rejected: it extends the overall deadline of an admission to
`unschedulableTimeout + timeout`, changes the meaning of `timeout` for existing users, and
ties the startup timer to an observation that may be delayed or missing. The agreed design
keeps `timeout` measured from the admission and only adds an earlier deadline.

### Workload controller reading `PodsScheduled` directly

Let the workload controller derive the state from `PodsScheduled` and `PodsReady` together,
with an evaluation order between the two conditions. Rejected in favor of a single source of
truth: the job framework, which already owns the `PodsReady` history, folds the observation
into the `PodsReady` reason, and the workload controller selects the timer from that reason
alone (reading `PodsScheduled` only for the start of the scheduling deadline).

### Resetting `PodsScheduled` from the admission patch

Reset `PodsScheduled` inside `UnsetQuotaReservationWithCondition`, atomically with the
`PodsReady` reset. Rejected: the condition would have to become an admission-managed
condition, and the scheduler's loose second-pass admission patch, taken from an earlier copy
of the Workload, could force-apply a stale `False` over a sticky `True`; the partial field
ownership of the admission manager could also break the CRD validation of the condition. The
tracker remains the only writer that decides the value.

### Removing `PodsScheduled` when the admission ends

Delete the condition instead of resetting it when a Workload loses its admission. Rejected
because it loses the history of the admission and the reason why the observation ended; the
removal is kept only for Workloads delegated to a MultiKueue worker, which are never observed
locally.

### A `SchedulingObserved` condition

A separate condition telling whether the last Pod list succeeded, to distinguish a stale
`PodsScheduled=False` from a fresh one. Rejected per maintainer feedback; the stale
observation is an accepted trade-off, and it cannot leak across admissions.

### A dedicated feature gate

Gate the tracker and the new reason behind a new feature gate. Rejected: the feature is an
extension of `waitForPodsReady`, which already has the `DisableWaitForPodsReady` gate and the
configuration block as opt-in; `unschedulableTimeout` itself opts into the shorter deadline.
