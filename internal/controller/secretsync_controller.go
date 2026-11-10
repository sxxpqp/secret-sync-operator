package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	syncv1 "github.com/sxxpqp/secret-sync-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	OwnerNamespace = "sync.secrets.io/owner-namespace"
	OwnerName      = "sync.secrets.io/owner-name"
	OwnerUID       = "sync.secrets.io/owner-uid"
	sourceIndex    = "spec.sourceSecretName"
	retryInterval  = 30 * time.Second
)

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=sync.secrets.io,resources=secretsyncs,verbs=get;list;watch
// +kubebuilder:rbac:groups=sync.secrets.io,resources=secretsyncs/status,verbs=get;update;patch

// SecretSyncReconciler reconciles a SecretSync object.
type SecretSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *SecretSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rule syncv1.SecretSync
	if err := r.Get(ctx, req.NamespacedName, &rule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rule.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	var source corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: rule.Namespace, Name: rule.Spec.SourceSecretName}, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return r.report(ctx, &rule, nil, "SourceNotFound", "Source Secret is missing; existing targets are retained")
		}
		return ctrl.Result{}, err
	}
	if source.Annotations[OwnerUID] != "" {
		return r.report(ctx, &rule, nil, "InvalidSource", "A managed target cannot be used as a source")
	}
	if source.Type == corev1.SecretTypeServiceAccountToken {
		return r.report(ctx, &rule, nil, "InvalidSource", "Service account token Secrets cannot be synchronized")
	}
	var synced []string
	for _, ns := range rule.Spec.TargetNamespaces {
		if ns == rule.Namespace {
			return r.report(ctx, &rule, synced, "InvalidTarget", "Target namespace must differ from the source namespace")
		}
	}
	var failures int
	for _, ns := range rule.Spec.TargetNamespaces {
		if err := r.syncTarget(ctx, &rule, &source, ns); err != nil {
			// Do not log API error bodies: a server response might contain Secret values.
			ctrl.LoggerFrom(ctx).Info("target synchronization failed", "namespace", ns, "secret", source.Name)
			failures++
			continue
		}
		synced = append(synced, ns)
	}
	if failures > 0 {
		return r.report(ctx, &rule, synced, "TargetSyncFailed", fmt.Sprintf("%d target(s) failed: check namespace existence, ownership, RBAC and immutability", failures))
	}
	return r.report(ctx, &rule, synced, "Synced", "All configured targets are synchronized")
}

func (r *SecretSyncReconciler) syncTarget(ctx context.Context, rule *syncv1.SecretSync, source *corev1.Secret, namespace string) error {
	var target corev1.Secret
	key := types.NamespacedName{Namespace: namespace, Name: source.Name}
	err := r.Get(ctx, key, &target)
	if apierrors.IsNotFound(err) {
		target = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Annotations: map[string]string{
				OwnerNamespace: rule.Namespace, OwnerName: rule.Name, OwnerUID: string(rule.UID),
			}},
			Type: source.Type, Data: source.DeepCopy().Data,
		}
		if err := r.Create(ctx, &target); err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("secret copied to target", "source", source.Name, "sourceNamespace", rule.Namespace, "targetNamespace", namespace)
		return nil
	}
	if err != nil {
		return err
	}
	if target.Annotations[OwnerNamespace] != rule.Namespace || target.Annotations[OwnerName] != rule.Name || target.Annotations[OwnerUID] != string(rule.UID) {
		return fmt.Errorf("target is owned by someone else")
	}
	if reflect.DeepEqual(target.Data, source.Data) && target.Type == source.Type {
		return nil
	}
	if target.Immutable != nil && *target.Immutable {
		return fmt.Errorf("target is immutable")
	}
	target.Data = source.DeepCopy().Data
	target.Type = source.Type
	// Do not copy ownerReferences, resourceVersion, source labels or annotations.
	// No cross-namespace owner reference: deletion always retains target copies.
	if err := r.Update(ctx, &target); err != nil {
		return err
	}
	ctrl.LoggerFrom(ctx).Info("secret updated in target", "source", source.Name, "sourceNamespace", rule.Namespace, "targetNamespace", namespace)
	return nil
}

func (r *SecretSyncReconciler) report(ctx context.Context, rule *syncv1.SecretSync, synced []string, reason, message string) (ctrl.Result, error) {
	before := rule.DeepCopy()
	status := metav1.ConditionFalse
	if reason == "Synced" {
		status = metav1.ConditionTrue
	}
	rule.Status.ObservedGeneration = rule.Generation
	rule.Status.SyncedNamespaces = synced
	meta.SetStatusCondition(&rule.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: status, Reason: reason, Message: message, ObservedGeneration: rule.Generation,
	})
	if !reflect.DeepEqual(before.Status, rule.Status) {
		if err := r.Status().Patch(ctx, rule, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Retry conflicts, namespace recreation and missed events without hot loops.
	return ctrl.Result{RequeueAfter: retryInterval}, nil
}

func (r *SecretSyncReconciler) requestsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	requests := map[types.NamespacedName]struct{}{}
	var rules syncv1.SecretSyncList
	if err := r.List(ctx, &rules, client.InNamespace(obj.GetNamespace()), client.MatchingFields{sourceIndex: obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Info("cannot list source rules; periodic reconciliation will retry")
	} else {
		for _, rule := range rules.Items {
			requests[client.ObjectKeyFromObject(&rule)] = struct{}{}
		}
	}
	annotations := obj.GetAnnotations()
	if annotations[OwnerNamespace] != "" && annotations[OwnerName] != "" {
		requests[types.NamespacedName{Namespace: annotations[OwnerNamespace], Name: annotations[OwnerName]}] = struct{}{}
	}
	out := make([]reconcile.Request, 0, len(requests))
	for key := range requests {
		out = append(out, reconcile.Request{NamespacedName: key})
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
func (r *SecretSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &syncv1.SecretSync{}, sourceIndex, func(obj client.Object) []string {
		return []string{obj.(*syncv1.SecretSync).Spec.SourceSecretName}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&syncv1.SecretSync{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Complete(r)
}
