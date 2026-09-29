# Blue-Green Traffic-Role Controller — Architecture & Requirements

## Problem

argo-rollouts' built-in `ActiveMetadata` mechanism only labels the active
pod after `Status.StableRS` updates, which is gated behind
`PostPromotionAnalysis` success. That means green pods can receive live
traffic for the full duration of post-promotion analysis before they're
labeled active. This controller labels pods the moment traffic actually
switches, by reading `rollout.status.blueGreen.activeSelector` directly —
this flips immediately at cutover, independent of `StableRS`.

## Requirements

- Watch Argo Rollouts `Rollout` resources cluster- or namespace-scoped.
- Only act on Rollouts that:
  - use the blue-green strategy (`Spec.Strategy.BlueGreen != nil`), and
  - carry an opt-in annotation (default `traffic-role-watcher/enabled: "true"`).
- Read `Status.BlueGreen.ActiveSelector` (the `rollouts-pod-template-hash`
  of the currently active ReplicaSet).
- For every ReplicaSet owned by the Rollout:
  - if its `rollouts-pod-template-hash` matches `ActiveSelector` → label
    with `traffic-role=active` (default label key/value, both configurable).
  - otherwise → remove the label if present.
- Apply the label change in two places, since Kubernetes never
  retroactively relabels existing pods when a template changes:
  1. Strategic-merge patch on the ReplicaSet's `spec.template.metadata.labels`
     (so future pods inherit it).
  2. Patch each currently-running pod owned by that ReplicaSet directly.
- Self-heal automatically: recomputing desired state from the *current*
  `ActiveSelector` on every reconcile means abort/rollback (which reverts
  `ActiveSelector` to the prior stable hash) naturally strips the label from
  reverted pods with no special-cased rollback logic.
- Clean up on disable: if a Rollout is edited to remove the opt-in
  annotation or drop the blue-green strategy, strip `traffic-role` from
  every owned ReplicaSet/pod rather than leaving stale labels forever.
- Configurable (CLI flags, sane defaults):
  - opt-in annotation key
  - label key
  - label value for "active"
- RBAC (least privilege):
  - `argoproj.io/rollouts`: get, list, watch
  - `argoproj.io/rollouts/status`: get
  - `apps/replicasets`: get, list, watch, patch
  - `""/pods`: get, list, watch, patch
- No CRD of our own — `Rollout` is an external type owned by argo-rollouts.

## Architecture

Built with **kubebuilder v4 / controller-runtime** (not a hand-rolled
client-go `SharedInformer`), scaffolded via:

```
kubebuilder init --domain <domain> --repo <module>
kubebuilder create api --group argoproj --version v1alpha1 --kind Rollout --resource=false --controller=true
```

`--resource=false` is essential: `Rollout` already exists as an argo-rollouts
CRD, so kubebuilder must scaffold only a controller/reconciler for the
externally-owned type, not generate a new CRD.

### Key components

- **`cmd/main.go`** — manager entrypoint. Registers
  `rolloutsv1alpha1.AddToScheme` (from
  `github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1`) alongside
  the client-go scheme. Exposes the three configuration knobs as flags
  (`--enabled-annotation`, `--traffic-role-label`,
  `--traffic-role-active-value`) and wires them into the reconciler.

- **`internal/controller/rollout_controller.go`** — the reconciler:
  - `RolloutReconciler` struct: `client.Client`, `Scheme`, plus the three
    configurable string fields, defaulted via `setDefaults()` if left unset.
  - `isWatchedRollout(enabledAnnotation string) predicate.Funcs` — a
    **custom** `predicate.Funcs` (not `predicate.NewPredicateFuncs`)
    filtering events before they reach the workqueue. Its `UpdateFunc`
    checks `matches(ObjectOld) || matches(ObjectNew)` rather than only the
    new object — this is what lets a *disable* transition (annotation
    removed / strategy dropped) still get enqueued once, instead of the
    default new-object-only semantics silently swallowing it.
  - `Reconcile`:
    1. Get the Rollout; NotFound → no-op.
    2. Compute `watched := BlueGreen != nil && annotation == "true"`.
    3. If watched: read `ActiveSelector`; if empty (not yet promoted),
       return early (nothing labeled yet).
       If not watched: leave `activeHash` as `""` and fall through — this
       makes the same label loop below double as the cleanup path, since
       every RS/pod will fail to match an empty hash and get unlabeled.
    4. List ReplicaSets owned by the Rollout (via `Spec.Selector`).
    5. For each RS: compute desired label value, patch the RS template if
       it changed, then patch each pod owned by that RS individually
       (ownership checked via `OwnerReferences` UID match).
    6. Aggregate and return errors via `errors.Join` — triggers
       controller-runtime's built-in requeue-with-backoff.
  - Periodic resync (controller-runtime cache resync) fires the same
    `Update` event path — confirmed via
    `sigs.k8s.io/controller-runtime/pkg/internal/source.EventHandler.OnUpdate`,
    which applies the identical predicate chain as any real update — so
    resync acts as a safety net without needing separate handling.
  - `SetupWithManager`: `For(&rolloutsv1alpha1.Rollout{}, builder.WithPredicates(isWatchedRollout(...)))`.
    No `.Owns()` for ReplicaSets/Pods — those are read/patched directly
    inside `Reconcile`, not watched as a separate event source.

### Dependencies / versioning notes (from real-world friction hit this
session, worth preserving)

- `github.com/argoproj/argo-rollouts` pins its own internal k8s.io
  sub-packages (`k8s.io/kubelet`, `k8s.io/cloud-provider`, etc.) at the
  placeholder version `v0.0.0`, resolved only via `replace` directives in
  argo-rollouts' *own* `go.mod` — which do **not** propagate to a consuming
  module. Any consumer must copy the relevant `replace` lines (matching
  argo-rollouts' pinned k8s release, e.g. `v0.29.3` for argo-rollouts
  v1.8.x) into its own `go.mod`, or `go mod tidy` / `go list -m all` /
  IDE module sync fails with `invalid version: unknown revision v0.0.0`.
  `go build`/`go vet` on code that doesn't import the affected packages can
  still succeed without the replaces — only full-graph resolution needs them.
- Confirm the actual package path before depending on it:
  `github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1` (note:
  `rollouts`, plural — not `rollout`).
- kubebuilder v4.16.0 scaffolds against `sigs.k8s.io/controller-runtime`
  v0.25.0, which requires `k8s.io/client-go` v0.37.0 — newer than the
  v0.34.5 that argo-rollouts v1.10.0 pins via its own `replace` directives.
  Building against the newer client-go while forced down to v0.34.5 via
  `replace` fails at compile time (`undefined: toolscache.DoneChecker`,
  `undefined: events.AnnotatedEventRecorder` — APIs added after v0.34).
  Fix: downgrade `sigs.k8s.io/controller-runtime` to the release whose own
  `go.mod` requires client-go v0.34.x (v0.22.5, requires v0.34.3) so both
  dependencies agree on the same client-go minor version.

## Testing

Implemented in `internal/controller/rollout_controller_test.go` as plain
Go table-driven tests against `sigs.k8s.io/controller-runtime/pkg/client/fake`
(no envtest binaries required — faster and more portable than the Ginkgo
envtest suite the kubebuilder scaffold generates, which was removed).

Cases covered: active RS/pods get labeled and the previously-active RS/pods
get unlabeled in the same reconcile; not-yet-promoted Rollout (empty
`ActiveSelector`) is a no-op; disabling the opt-in annotation strips stale
labels; dropping the blue-green strategy strips stale labels; abort/rollback
(`ActiveSelector` reverts to a prior hash) relabels correctly with no
special-cased rollback logic — the same code path that labels on promotion
also relabels on rollback.
