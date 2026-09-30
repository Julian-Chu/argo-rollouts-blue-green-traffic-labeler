/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	defaultEnabledAnnotation  = "traffic-active-watcher/enabled"
	defaultTrafficActiveLabel = "traffic-active"
	defaultTrafficActiveValue = "true"
)

// RolloutReconciler reconciles a Rollout object, labeling the pods and
// ReplicaSets that are currently receiving live traffic in a blue-green
// rollout, independent of argo-rollouts' own ActiveMetadata mechanism.
type RolloutReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// EnabledAnnotation is the opt-in annotation key checked on the Rollout.
	EnabledAnnotation string
	// TrafficActiveLabel is the label key applied to the active RS/pods.
	TrafficActiveLabel string
	// TrafficActiveValue is the label value applied to the active RS/pods.
	TrafficActiveValue string
}

// setDefaults fills any unset configuration fields with their defaults.
func (r *RolloutReconciler) setDefaults() {
	if r.EnabledAnnotation == "" {
		r.EnabledAnnotation = defaultEnabledAnnotation
	}
	if r.TrafficActiveLabel == "" {
		r.TrafficActiveLabel = defaultTrafficActiveLabel
	}
	if r.TrafficActiveValue == "" {
		r.TrafficActiveValue = defaultTrafficActiveValue
	}
}

// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=rollouts/status,verbs=get
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch

// Reconcile labels the ReplicaSet and Pods that match the Rollout's current
// blue-green ActiveSelector with the configured traffic-active label, and
// strips the label from every other RS/pod owned by the Rollout.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var rollout rolloutsv1alpha1.Rollout
	if err := r.Get(ctx, req.NamespacedName, &rollout); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	watched := isWatched(&rollout, r.EnabledAnnotation)

	// activeHash stays "" when the Rollout isn't watched, or hasn't been
	// promoted yet. Every RS/pod owned by the Rollout will then fail to
	// match it and get unlabeled, which doubles as the cleanup path for
	// disabled/reverted Rollouts with no special-cased logic.
	var activeHash string
	if watched {
		activeHash = rollout.Status.BlueGreen.ActiveSelector
		if activeHash == "" {
			// Not yet promoted: nothing has been labeled, nothing to do.
			return ctrl.Result{}, nil
		}
	}

	log.Info("Reconciling Rollout traffic-active labels", "rollout", rollout.Name, "watched", watched, "activeSelector", activeHash)

	selector, err := metav1.LabelSelectorAsSelector(rollout.Spec.Selector)
	if err != nil {
		return ctrl.Result{}, err
	}

	var replicaSets appsv1.ReplicaSetList
	if err := r.List(ctx, &replicaSets, client.InNamespace(rollout.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return ctrl.Result{}, err
	}

	var errs []error
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		if !isOwnedBy(rs.OwnerReferences, rollout.UID) {
			continue
		}

		desired := ""
		if activeHash != "" && rs.Labels[rolloutsv1alpha1.DefaultRolloutUniqueLabelKey] == activeHash {
			desired = r.TrafficActiveValue
		}

		if err := r.patchReplicaSetLabel(ctx, rs, desired); err != nil {
			errs = append(errs, err)
			continue
		}

		if err := r.relabelPods(ctx, rs, desired); err != nil {
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		log.Error(err, "failed to reconcile traffic-active labels")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// isWatched reports whether a Rollout uses the blue-green strategy and
// carries the opt-in annotation set to "true".
func isWatched(rollout *rolloutsv1alpha1.Rollout, enabledAnnotation string) bool {
	return rollout.Spec.Strategy.BlueGreen != nil && rollout.Annotations[enabledAnnotation] == "true"
}

func isOwnedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// patchReplicaSetLabel strategic-merge-patches the RS's pod template label
// so that future pods created from it inherit the desired traffic-active
// value. Kubernetes never retroactively relabels existing pods when a
// template changes, so relabelPods handles the currently-running pods.
func (r *RolloutReconciler) patchReplicaSetLabel(ctx context.Context, rs *appsv1.ReplicaSet, desired string) error {
	current := rs.Spec.Template.Labels[r.TrafficActiveLabel]
	if current == desired {
		return nil
	}

	patch := client.MergeFrom(rs.DeepCopy())
	if desired == "" {
		delete(rs.Spec.Template.Labels, r.TrafficActiveLabel)
	} else {
		if rs.Spec.Template.Labels == nil {
			rs.Spec.Template.Labels = map[string]string{}
		}
		rs.Spec.Template.Labels[r.TrafficActiveLabel] = desired
	}
	if err := r.Patch(ctx, rs, patch); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Patched ReplicaSet traffic-active label", "replicaSet", rs.Name, "from", current, "to", desired)
	return nil
}

// relabelPods patches every currently-running pod owned by rs to carry (or
// no longer carry) the traffic-active label.
func (r *RolloutReconciler) relabelPods(ctx context.Context, rs *appsv1.ReplicaSet, desired string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(rs.Namespace), client.MatchingLabels(rs.Spec.Selector.MatchLabels)); err != nil {
		return err
	}

	log := logf.FromContext(ctx)

	var errs []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !isOwnedBy(pod.OwnerReferences, rs.UID) {
			continue
		}
		current := pod.Labels[r.TrafficActiveLabel]
		if current == desired {
			continue
		}

		patch := client.MergeFrom(pod.DeepCopy())
		if desired == "" {
			delete(pod.Labels, r.TrafficActiveLabel)
		} else {
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
			}
			pod.Labels[r.TrafficActiveLabel] = desired
		}
		if err := r.Patch(ctx, pod, patch); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Info("Patched Pod traffic-active label", "pod", pod.Name, "replicaSet", rs.Name, "from", current, "to", desired)
	}
	return errors.Join(errs...)
}

// isWatchedRollout is a predicate that only enqueues Rollouts that are (or
// were) watched. Its UpdateFunc checks matches(ObjectOld) || matches(ObjectNew)
// rather than only the new object: this is what lets a *disable* transition
// (annotation removed / strategy dropped) still get enqueued once, instead
// of the default new-object-only semantics silently swallowing it.
func isWatchedRollout(enabledAnnotation string) predicate.Funcs {
	matches := func(obj client.Object) bool {
		rollout, ok := obj.(*rolloutsv1alpha1.Rollout)
		if !ok {
			return false
		}
		return isWatched(rollout, enabledAnnotation)
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return matches(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return matches(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return matches(e.ObjectOld) || matches(e.ObjectNew) },
		GenericFunc: func(e event.GenericEvent) bool { return matches(e.Object) },
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.setDefaults()
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutsv1alpha1.Rollout{}, builder.WithPredicates(isWatchedRollout(r.EnabledAnnotation))).
		Named("rollout").
		Complete(r)
}
