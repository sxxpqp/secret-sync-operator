package controller

import (
	"context"
	"testing"

	syncv1 "github.com/sxxpqp/secret-sync-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var ctx = context.Background()
var ruleKey = types.NamespacedName{Namespace: "source", Name: "copy-registry"}
var targetKey = types.NamespacedName{Namespace: "prod", Name: "registry"}

func fixture(t *testing.T) (*SecretSyncReconciler, *syncv1.SecretSync) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := syncv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	rule := &syncv1.SecretSync{
		ObjectMeta: metav1.ObjectMeta{Namespace: ruleKey.Namespace, Name: ruleKey.Name, UID: "rule-uid", Generation: 1},
		Spec:       syncv1.SecretSyncSpec{SourceSecretName: "registry", TargetNamespaces: []string{"prod"}},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "source", Name: "registry", Labels: map[string]string{"private": "do-not-copy"}},
		Type:       corev1.SecretTypeOpaque, Data: map[string][]byte{"password": []byte("first")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&syncv1.SecretSync{}).
		WithIndex(&syncv1.SecretSync{}, sourceIndex, func(obj client.Object) []string { return []string{obj.(*syncv1.SecretSync).Spec.SourceSecretName} }).
		WithObjects(rule, source).Build()
	return &SecretSyncReconciler{Client: c, Scheme: scheme}, rule
}
func runReconcile(t *testing.T, r *SecretSyncReconciler) {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: ruleKey}); err != nil {
		t.Fatal(err)
	}
}
func getTarget(t *testing.T, r *SecretSyncReconciler) *corev1.Secret {
	t.Helper()
	var target corev1.Secret
	if err := r.Get(ctx, targetKey, &target); err != nil {
		t.Fatal(err)
	}
	return &target
}
func assertReady(t *testing.T, r *SecretSyncReconciler, want bool) {
	t.Helper()
	var rule syncv1.SecretSync
	if err := r.Get(ctx, ruleKey, &rule); err != nil {
		t.Fatal(err)
	}
	if meta.IsStatusConditionTrue(rule.Status.Conditions, "Ready") != want {
		t.Fatalf("unexpected status: %+v", rule.Status)
	}
}

func TestCreateUpdateRepairAndRecreate(t *testing.T) {
	r, _ := fixture(t)
	runReconcile(t, r)
	target := getTarget(t, r)
	if string(target.Data["password"]) != "first" || len(target.OwnerReferences) != 0 || len(target.Labels) != 0 {
		t.Fatal("incorrect target copy or metadata")
	}
	assertReady(t, r, true)
	// Source update propagates, including removal of obsolete keys and type changes.
	var source corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: "source", Name: "registry"}, &source); err != nil {
		t.Fatal(err)
	}
	source.Data = map[string][]byte{"username": []byte("second")}
	source.Type = corev1.SecretTypeBasicAuth
	source.Data["password"] = []byte("updated")
	if err := r.Update(ctx, &source); err != nil {
		t.Fatal(err)
	}
	requests := r.requestsForSecret(ctx, &source)
	if len(requests) != 1 || requests[0].NamespacedName != ruleKey {
		t.Fatalf("source not enqueued: %v", requests)
	}
	runReconcile(t, r)
	target = getTarget(t, r)
	if target.Type != source.Type || string(target.Data["username"]) != "second" {
		t.Fatal("source update not synchronized")
	}
	version := target.ResourceVersion
	runReconcile(t, r)
	if getTarget(t, r).ResourceVersion != version {
		t.Fatal("unchanged target was rewritten")
	}
	target.Data["password"] = []byte("tampered")
	if err := r.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if string(getTarget(t, r).Data["password"]) != "updated" {
		t.Fatal("target drift not repaired")
	}
	target = getTarget(t, r)
	if len(r.requestsForSecret(ctx, target)) != 1 {
		t.Fatal("target change not enqueued")
	}
	if err := r.Delete(ctx, target); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if string(getTarget(t, r).Data["password"]) != "updated" {
		t.Fatal("deleted target not recreated")
	}
}
func TestUnmanagedAndPreviousOwnerTargetsAreNeverOverwritten(t *testing.T) {
	for _, annotations := range []map[string]string{nil, {OwnerNamespace: "source", OwnerName: ruleKey.Name, OwnerUID: "old-rule-uid"}} {
		r, _ := fixture(t)
		target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "registry", Annotations: annotations}, Data: map[string][]byte{"keep": []byte("original")}}
		if err := r.Create(ctx, target); err != nil {
			t.Fatal(err)
		}
		runReconcile(t, r)
		if string(getTarget(t, r).Data["keep"]) != "original" {
			t.Fatal("unowned Secret overwritten")
		}
		assertReady(t, r, false)
	}
}
func TestSourceAndRuleDeletionRetainTargets(t *testing.T) {
	r, rule := fixture(t)
	runReconcile(t, r)
	if err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "source", Name: "registry"}}); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	getTarget(t, r)
	assertReady(t, r, false)
	if err := r.Delete(ctx, rule); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	getTarget(t, r)
}
func TestImmutableTargetReportsFailureWithoutRecreation(t *testing.T) {
	r, _ := fixture(t)
	runReconcile(t, r)
	target := getTarget(t, r)
	yes := true
	target.Immutable = &yes
	target.Data["password"] = []byte("locked")
	if err := r.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if string(getTarget(t, r).Data["password"]) != "locked" {
		t.Fatal("immutable target modified")
	}
	assertReady(t, r, false)
}
func TestRejectsManagedAndServiceAccountSources(t *testing.T) {
	for _, managed := range []bool{false, true} {
		r, _ := fixture(t)
		var source corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: "source", Name: "registry"}, &source); err != nil {
			t.Fatal(err)
		}
		if managed {
			source.Annotations = map[string]string{OwnerUID: "another-rule"}
		} else {
			source.Type = corev1.SecretTypeServiceAccountToken
		}
		if err := r.Update(ctx, &source); err != nil {
			t.Fatal(err)
		}
		runReconcile(t, r)
		var target corev1.Secret
		if err := r.Get(ctx, targetKey, &target); err == nil {
			t.Fatal("invalid source was copied")
		}
		assertReady(t, r, false)
	}
}
func TestRemovingTargetRetainsCopyAndStopsSync(t *testing.T) {
	r, _ := fixture(t)
	runReconcile(t, r)
	var rule syncv1.SecretSync
	if err := r.Get(ctx, ruleKey, &rule); err != nil {
		t.Fatal(err)
	}
	rule.Spec.TargetNamespaces = []string{"test"}
	if err := r.Update(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if string(getTarget(t, r).Data["password"]) != "first" {
		t.Fatal("removed target not retained")
	}
	var other corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: "test", Name: "registry"}, &other); err != nil {
		t.Fatal(err)
	}
}
