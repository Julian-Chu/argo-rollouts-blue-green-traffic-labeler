# Controller Review — `internal/controller/rollout_controller.go`

Two independent reviews (Kubernetes expert perspective, principal software engineer perspective). Review-only, no fixes applied.

## Consensus critical finding

**Controller doesn't watch what it labels.** `SetupWithManager` (rollout_controller.go:240-246) only does `.For(&rolloutsv1alpha1.Rollout{})` — no `.Owns(&appsv1.ReplicaSet{})` / `.Owns(&corev1.Pod{})` or an equivalent `Watches`. Reconcile only fires on Rollout create/update/delete/generic events, so it's edge-triggered off Rollout changes rather than level-triggered against the resources it actually mutates.

- Pods that appear, restart, get evicted, or get rescheduled *after* the triggering Rollout event (HPA scale-up, node drain, `kubectl delete pod`, crash-loop restarts) never get labeled and never self-heal.
- This is the core gap for a controller whose entire job is keeping `traffic-role` labels in sync with active pods.
- Fix: add `.Owns(&appsv1.ReplicaSet{})` and a `Watches` on Pods mapped back to the owning Rollout (gated by the same `isWatchedRollout`-style filter to avoid a watch storm), or a `RequeueAfter` as a cheaper stopgap.

## Other findings (k8s review)

1. **Delete leaks labels — narrower than first stated.** `rollout_controller.go:84-89`: on Rollout delete, `Reconcile` does `Get` → `IsNotFound` → returns `nil` before touching any RS/pod. Originally flagged as a general leak, but Kubernetes' own garbage collector already handles the common case: Rollout owns RS via `OwnerReference`, RS owns Pods, so a normal delete cascades and removes the RS/Pods along with the Rollout — no live object survives to carry a stale label. The leak only happens when that cascade doesn't run:
   - `kubectl delete rollout --cascade=orphan` (or `propagationPolicy: Orphan`) — RS/Pods are explicitly detached instead of deleted, survive with `traffic-role=active` still set, and nothing reconciles them again since the Rollout is gone.
   - The RS has its own finalizer blocking GC — it lingers, still labeled, after this controller has stopped reconciling.

   Real, but a narrow edge case (explicit orphan deletion or a stuck RS finalizer), not a per-delete leak. Fix (finalizer on the Rollout, or an owner-UID sweep on delete) only worth doing if orphan-deletion of Rollouts is an actual practice in this environment.

2. **Selector matching drops `matchExpressions`** — `relabelPods:187` uses `client.MatchingLabels(rs.Spec.Selector.MatchLabels)`, ignoring any `MatchExpressions` on the RS selector. Low real-world risk (Argo Rollouts RS selectors are typically equality-only today) but silently wrong if that changes. Should build a real selector via `metav1.LabelSelectorAsSelector(rs.Spec.Selector)`, as already done for the Rollout.

3. **No explicit `RequeueAfter`** on transient errors — relies entirely on controller-runtime's default rate-limited backoff. Not necessarily wrong, just worth confirming it's intentional.

## Other findings (principal engineer review)

- `relabelPods` (line 187-196) lists pods by `rs.Spec.Selector.MatchLabels` then filters by owner UID — a broader `List` than strictly necessary, harmless given pod-template-hash uniqueness. Minor, non-blocking.

## What's solid (both reviews agree)

- `isWatched`/predicate disable-transition handling (lines 218-236) — correct and non-obvious, with a good comment explaining why `ObjectOld || ObjectNew` matters.
- Cleanup-via-empty-`activeHash`: unlabeling everything when `activeHash` is empty cleanly unifies disable/revert/not-yet-promoted into one code path.
- Idempotent patches — early-return when `current == desired` avoids needless API writes.
- Owner-UID filtering when selecting pods/ReplicaSets.
- `errors.Join` + continue-on-error per RS — one bad RS doesn't block reconciliation of the others.
- `MergeFrom`-based patches.

## Verdict

Both reviews, run independently, converged on the same top issue: missing `Owns()`/`Watches` on ReplicaSets and Pods. This is the priority fix. The delete-leak and `matchExpressions` gaps are secondary and lower severity.

## Follow-up: do we actually need to watch Pod/RS?

Not necessarily — the two reviews may have overstated it. Argo Rollouts' own controller is a fast-resyncing, level-based loop that continuously writes `Rollout.Status` (`readyReplicas`, `availableReplicas`, `updatedReplicas`, etc.) whenever an owned RS/Pod changes. Since `isWatchedRollout`'s `UpdateFunc` fires on **any** Rollout update (not a specific field), most pod churn — eviction, crash restart, HPA scale, node drain — already bubbles up into a Rollout status write and re-triggers this controller indirectly.

**Where the gap is real:** if a pod appears/disappears in a way that doesn't touch any Rollout status field, or Argo Rollouts' own reconcile is delayed, this controller has no independent trigger and drifts until the next real Rollout event.

**Cost of the full fix:** `Owns(&ReplicaSet{})` + `Watches` on Pods is real weight — more RBAC, watch cache overhead, and cache-manager wiring, plus it still needs `isWatchedRollout`-equivalent filtering on RS/Pod events to avoid a watch storm across every pod in the cluster. That's a lot of machinery to close a narrow, mostly-already-covered edge case.

**Cheaper fix, same self-healing property:** return `ctrl.Result{RequeueAfter: someInterval}` from `Reconcile` whenever `watched` is true. Periodic self-correction with bounded staleness (= the interval), no new watches, RBAC, or predicates.

```go
return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
```

Skip full `Owns()`/`Watches`; add real watches only if sub-second correction is required instead of bounded eventual consistency.

## Second-pass reviews (read this doc first)

Two more reviews (k8s expert, principal engineer) re-examined the controller after reading the above. Both sign off on the `RequeueAfter` call over full `Owns()`/`Watches`, and both raise the same correction.

**Correction: `RequeueAfter` does not fix the delete-leak (finding #1).** The early-return at line 84-89 (`Get` → `IsNotFound` → `return nil`) happens *before* any `ctrl.Result` is constructed. A deleted Rollout gets no further reconciles at all — periodic requeue on *other* watched Rollouts does nothing for it. This is orthogonal to the watch/requeue question — but per the narrowing above, it's also a narrow edge case (orphan-cascade deletion or a stuck RS finalizer), not a general leak, since normal Kubernetes GC cascade-deletes owned RS/Pods along with the Rollout. Only worth a dedicated fix (finalizer or owner-UID sweep) if orphan deletion is actually used here. Likewise, the `matchExpressions` gap (#2) is untouched by `RequeueAfter` — separate, still low severity.

**New finding: both known bugs are also test-coverage gaps.** None of the five existing tests (`rollout_controller_test.go:143-202`) exercise Rollout deletion or a selector using `matchExpressions`. Worth adding regression tests alongside whichever fix lands first, rather than as a separate follow-up.

### Updated priority order

1. `RequeueAfter` self-heal for the residual pod/RS drift gap — cheap, already validated by two independent reviews.
2. `matchExpressions` selector fix in `relabelPods` (`LabelSelectorAsSelector`, matching the pattern already used for the Rollout selector) — one-line consistency fix, low severity, no test coverage.
3. Delete-leak fix (finalizer or owner-UID sweep on delete) — downgraded: normal K8s GC cascade already deletes owned RS/Pods along with the Rollout, so this only matters for explicit orphan-deletion or a stuck RS finalizer. Only worth doing if that's an actual practice here.
4. Full `Owns()`/`Watches` on ReplicaSet/Pod — not recommended; disproportionate to the residual gap `RequeueAfter` already covers.
