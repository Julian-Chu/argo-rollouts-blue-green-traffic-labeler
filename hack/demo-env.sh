#!/usr/bin/env bash
# Stand up / tear down a local kind cluster running Argo Rollouts + this
# controller + a sample blue-green Rollout. See `make demo-up` / `make demo-down`.
set -euo pipefail

KIND_CLUSTER=${KIND_CLUSTER:-argo-rollouts-blue-green-traffic-labeler-demo}
IMG=${IMG:-controller:dev}
KIND=${KIND:-kind}
KUBECTL=${KUBECTL:-kubectl}
KUSTOMIZE=${KUSTOMIZE:-kustomize}
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

up() {
  case "$("$KIND" get clusters)" in
    *"$KIND_CLUSTER"*)
      echo "Kind cluster '$KIND_CLUSTER' already exists. Skipping creation." ;;
    *)
      echo "Creating kind cluster '$KIND_CLUSTER'..."
      "$KIND" create cluster --name "$KIND_CLUSTER" ;;
  esac

  echo "Installing Argo Rollouts..."
  "$KUBECTL" create namespace argo-rollouts --dry-run=client -o yaml | "$KUBECTL" apply -f -
  # --server-side: the rollouts/analysisruns CRDs exceed the last-applied-configuration
  # annotation size limit that client-side `apply` enforces.
  "$KUBECTL" apply --server-side --force-conflicts -n argo-rollouts \
    -f https://github.com/argoproj/argo-rollouts/releases/latest/download/install.yaml
  "$KUBECTL" wait --for=condition=available --timeout=180s deployment/argo-rollouts -n argo-rollouts

  echo "Installing Argo Rollouts dashboard..."
  "$KUBECTL" apply --server-side --force-conflicts -n argo-rollouts \
    -f https://github.com/argoproj/argo-rollouts/releases/latest/download/dashboard-install.yaml
  "$KUBECTL" wait --for=condition=available --timeout=180s deployment/argo-rollouts-dashboard -n argo-rollouts

  echo "Building and loading controller image '$IMG'..."
  docker build -t "$IMG" "$ROOT_DIR"
  "$KIND" load docker-image "$IMG" --name "$KIND_CLUSTER"

  echo "Deploying controller..."
  (cd "$ROOT_DIR/config/manager" && "$KUSTOMIZE" edit set image controller="$IMG")
  "$KUSTOMIZE" build "$ROOT_DIR/config/default" | "$KUBECTL" apply -f -
  "$KUBECTL" rollout status deployment/argo-rollouts-blue-green-traffic-labeler-controller-manager \
    -n argo-rollouts-blue-green-traffic-labeler-system

  echo "Applying sample blue-green Rollout..."
  "$KUBECTL" apply -f "$ROOT_DIR/config/samples/rollout-bluegreen-demo.yaml"

  echo "Done. Try: kubectl get pods -l traffic-role=active"
  echo "Dashboard: kubectl port-forward -n argo-rollouts svc/argo-rollouts-dashboard 3100:3100"
}

down() {
  "$KIND" delete cluster --name "$KIND_CLUSTER"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *) echo "Usage: $0 {up|down}" >&2; exit 1 ;;
esac
