# argo-rollouts-blue-green-traffic-labeler
add label for blue green strategy traffic switch

## Local demo

Prerequisites: `kind`, `docker`, `kubectl` (and optionally the
[`kubectl-argo-rollouts`](https://argoproj.github.io/argo-rollouts/installation/#kubectl-plugin-installation)
plugin, for promoting the rollout).

```
make demo-up    # kind cluster + Argo Rollouts + this controller + a sample blue-green Rollout
make demo-down  # tear down the kind cluster
```

Exercise it:

```
kubectl get pods -l traffic-role=active                     # active pods labeled after first rollout
kubectl argo rollouts set image demo demo=nginxinc/nginx-unprivileged:1.26
kubectl argo rollouts promote demo                           # requires kubectl-argo-rollouts plugin
kubectl get pods -l traffic-role=active -o wide              # label moved to the new active RS's pods
```
