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
	"testing"

	rolloutsv1alpha1 "github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace  = "default"
	testAppLabel   = "app"
	testAppValue   = "demo"
	testRolloutUID = types.UID("rollout-uid")
	testRSUID      = types.UID("rs-uid")
	testRS2UID     = types.UID("rs2-uid")
	testEnabled    = "true"
	testHashA      = "hash-a"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rolloutsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newRollout(annotations map[string]string, blueGreen bool, activeSelector string) *rolloutsv1alpha1.Rollout {
	rollout := &rolloutsv1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "demo",
			Namespace:   testNamespace,
			UID:         testRolloutUID,
			Annotations: annotations,
		},
		Spec: rolloutsv1alpha1.RolloutSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{testAppLabel: testAppValue}},
		},
	}
	if blueGreen {
		rollout.Spec.Strategy.BlueGreen = &rolloutsv1alpha1.BlueGreenStrategy{}
	}
	rollout.Status.BlueGreen.ActiveSelector = activeSelector
	return rollout
}

func newReplicaSet(name string, uid types.UID, hash string, templateLabels map[string]string) *appsv1.ReplicaSet {
	labels := map[string]string{
		testAppLabel: testAppValue,
		rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: hash,
	}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			UID:       uid,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				{UID: testRolloutUID},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: templateLabels},
			},
		},
	}
}

func newPod(name string, ownerUID types.UID, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{
				{UID: ownerUID},
			},
		},
	}
}

func reconcile(t *testing.T, c client.Client) {
	t.Helper()
	r := &RolloutReconciler{Client: c}
	r.setDefaults()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "demo"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func assertLabel(t *testing.T, c client.Client, rsName string, wantRSLabel string, podName string, wantPodLabel string) {
	t.Helper()

	var rs appsv1.ReplicaSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: rsName}, &rs); err != nil {
		t.Fatalf("get ReplicaSet %s: %v", rsName, err)
	}
	if got := rs.Spec.Template.Labels[defaultTrafficRoleLabel]; got != wantRSLabel {
		t.Errorf("ReplicaSet %s template label = %q, want %q", rsName, got, wantRSLabel)
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: podName}, &pod); err != nil {
		t.Fatalf("get Pod %s: %v", podName, err)
	}
	if got := pod.Labels[defaultTrafficRoleLabel]; got != wantPodLabel {
		t.Errorf("Pod %s label = %q, want %q", podName, got, wantPodLabel)
	}
}

func TestReconcile_ActiveAndInactiveGetLabeledCorrectly(t *testing.T) {
	scheme := newScheme(t)
	rollout := newRollout(map[string]string{defaultEnabledAnnotation: testEnabled}, true, testHashA)
	activeRS := newReplicaSet("demo-a", testRSUID, testHashA, nil)
	inactiveRS := newReplicaSet("demo-b", testRS2UID, "hash-b", map[string]string{defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})
	activePod := newPod("demo-a-pod", testRSUID, map[string]string{testAppLabel: testAppValue, rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: testHashA})
	inactivePod := newPod("demo-b-pod", testRS2UID, map[string]string{testAppLabel: testAppValue, rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: "hash-b", defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rollout, activeRS, inactiveRS, activePod, inactivePod).
		Build()

	reconcile(t, c)

	assertLabel(t, c, "demo-a", "active", "demo-a-pod", "active")
	assertLabel(t, c, "demo-b", "", "demo-b-pod", "")
}

func TestReconcile_NotYetPromotedIsNoOp(t *testing.T) {
	scheme := newScheme(t)
	rollout := newRollout(map[string]string{defaultEnabledAnnotation: testEnabled}, true, "")
	rs := newReplicaSet("demo-a", testRSUID, testHashA, nil)
	pod := newPod("demo-a-pod", testRSUID, map[string]string{testAppLabel: testAppValue})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout, rs, pod).Build()

	reconcile(t, c)

	assertLabel(t, c, "demo-a", "", "demo-a-pod", "")
}

func TestReconcile_DisabledRolloutCleansUpStaleLabels(t *testing.T) {
	scheme := newScheme(t)
	// No opt-in annotation: previously labeled RS/pods must be cleaned up.
	rollout := newRollout(nil, true, testHashA)
	rs := newReplicaSet("demo-a", testRSUID, testHashA, map[string]string{defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})
	pod := newPod("demo-a-pod", testRSUID, map[string]string{testAppLabel: testAppValue, rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: testHashA, defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout, rs, pod).Build()

	reconcile(t, c)

	assertLabel(t, c, "demo-a", "", "demo-a-pod", "")
}

func TestReconcile_NonBlueGreenRolloutCleansUpStaleLabels(t *testing.T) {
	scheme := newScheme(t)
	// Annotation still set, but blue-green strategy dropped.
	rollout := newRollout(map[string]string{defaultEnabledAnnotation: testEnabled}, false, testHashA)
	rs := newReplicaSet("demo-a", testRSUID, testHashA, map[string]string{defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})
	pod := newPod("demo-a-pod", testRSUID, map[string]string{testAppLabel: testAppValue, rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: testHashA, defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rollout, rs, pod).Build()

	reconcile(t, c)

	assertLabel(t, c, "demo-a", "", "demo-a-pod", "")
}

func TestReconcile_RollbackRelabelsWithoutSpecialCasing(t *testing.T) {
	scheme := newScheme(t)
	// ActiveSelector reverted from hash-b back to hash-a (abort/rollback).
	rollout := newRollout(map[string]string{defaultEnabledAnnotation: testEnabled}, true, testHashA)
	revertedActiveRS := newReplicaSet("demo-a", testRSUID, testHashA, nil)
	previouslyActiveRS := newReplicaSet("demo-b", testRS2UID, "hash-b", map[string]string{defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})
	revertedActivePod := newPod("demo-a-pod", testRSUID, map[string]string{testAppLabel: testAppValue, rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: testHashA})
	previouslyActivePod := newPod("demo-b-pod", testRS2UID, map[string]string{testAppLabel: testAppValue, rolloutsv1alpha1.DefaultRolloutUniqueLabelKey: "hash-b", defaultTrafficRoleLabel: defaultTrafficRoleActiveValue})

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(rollout, revertedActiveRS, previouslyActiveRS, revertedActivePod, previouslyActivePod).
		Build()

	reconcile(t, c)

	assertLabel(t, c, "demo-a", "active", "demo-a-pod", "active")
	assertLabel(t, c, "demo-b", "", "demo-b-pod", "")
}
