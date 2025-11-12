package reconcilergroup

import (
	"context"
	"github.com/otterize/intents-operator/src/shared/errors"
	"github.com/sirupsen/logrus"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"
)

type ReconcilerWithEvents interface {
	reconcile.Reconciler
	InjectRecorder(recorder record.EventRecorder)
}

type Group struct {
	reconcilers      []ReconcilerWithEvents
	name             string
	client           client.Client
	scheme           *runtime.Scheme
	recorder         record.EventRecorder
	baseObject       client.Object
	finalizer        string
	legacyFinalizers []string
}

func NewGroup(
	name string,
	client client.Client,
	scheme *runtime.Scheme,
	resourceObject client.Object,
	finalizer string,
	legacyFinalizers []string,
	reconcilers ...ReconcilerWithEvents,
) *Group {
	return &Group{
		reconcilers:      reconcilers,
		name:             name,
		client:           client,
		scheme:           scheme,
		baseObject:       resourceObject,
		finalizer:        finalizer,
		legacyFinalizers: legacyFinalizers,
	}
}

func (g *Group) AddToGroup(reconciler ReconcilerWithEvents) {
	g.reconcilers = append(g.reconcilers, reconciler)
}

func (g *Group) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	timeoutCtx, cancel := context.WithTimeoutCause(ctx, 70*time.Second, errors.Errorf("timeout while reconciling client intents %s", req.NamespacedName))
	defer cancel()
	ctx = timeoutCtx

	var finalErr error
	var finalRes ctrl.Result
	logrus.Debugf("## Starting reconciliation group cycle for %s, resource %s in namespace %s", g.name, req.Name, req.Namespace)

	resourceObject := g.baseObject.DeepCopyObject().(client.Object)
	err := g.client.Get(ctx, req.NamespacedName, resourceObject)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, errors.Wrap(err)
	}
	if k8serrors.IsNotFound(err) {
		logrus.Debugf("Resource %s not found, skipping reconciliation", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Log deletion status immediately after getting the resource
	deletionTimestamp := resourceObject.GetDeletionTimestamp()
	if deletionTimestamp != nil {
		logrus.Infof("DELETION DETECTED: Resource %s/%s is being deleted (deletionTimestamp: %v)", req.Namespace, req.Name, deletionTimestamp)
	} else {
		logrus.Debugf("Resource %s/%s is NOT being deleted (no deletionTimestamp)", req.Namespace, req.Name)
	}

	err = g.ensureFinalizer(ctx, resourceObject)
	if err != nil {
		if isKubernetesRaceRelatedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, errors.Wrap(err)
	}

	err = g.removeLegacyFinalizers(ctx, resourceObject)
	if err != nil {
		if isKubernetesRaceRelatedError(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, errors.Wrap(err)
	}

	objectBeingDeleted := resourceObject.GetDeletionTimestamp() != nil
	if objectBeingDeleted {
		logrus.Infof("BEFORE runGroup: Resource %s/%s has deletionTimestamp, will proceed with reconciliation then remove finalizer", req.Namespace, req.Name)
	}

	finalRes, finalErr = g.runGroup(ctx, req, finalErr, finalRes)

	// When object is being deleted, we should try to remove finalizer even if there were errors
	// during reconciliation. The errors might be from processing other resources (e.g., timeout
	// when patching network policies), and shouldn't block deletion of this resource.
	// The individual reconcilers should handle cleanup gracefully (e.g., treating NotFound as success).
	if objectBeingDeleted {
		logrus.Infof("AFTER runGroup: Attempting to remove finalizer for %s/%s (finalErr: %v, finalRes.IsZero: %v)", req.Namespace, req.Name, finalErr, finalRes.IsZero())
		if finalErr != nil {
			// Log the error but continue with finalizer removal
			logrus.WithError(finalErr).Warnf("Errors occurred during deletion reconciliation of %s/%s, attempting to remove finalizer anyway", req.Namespace, req.Name)
		}
		if !finalRes.IsZero() {
			// Requeue was requested, but we still try to remove finalizer
			logrus.Debugf("Requeue requested for deleting resource %s/%s, attempting to remove finalizer anyway", req.Namespace, req.Name)
		}

		err = g.removeFinalizer(ctx, resourceObject)
		if err != nil {
			if isKubernetesRaceRelatedError(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, errors.Wrap(err)
		}

		// Successfully removed finalizer, resource can now be deleted by Kubernetes
		logrus.Debugf("Successfully removed finalizer for %s/%s", req.Namespace, req.Name)
		return ctrl.Result{}, nil
	}

	return finalRes, finalErr
}

func (g *Group) removeLegacyFinalizers(ctx context.Context, resource client.Object) error {
	shouldUpdate := false
	for _, legacyFinalizer := range g.legacyFinalizers {
		if controllerutil.ContainsFinalizer(resource, legacyFinalizer) {
			controllerutil.RemoveFinalizer(resource, legacyFinalizer)
			shouldUpdate = true
		}
	}

	if shouldUpdate {
		err := g.client.Update(ctx, resource)
		if err != nil {
			return errors.Errorf("failed to remove legacy finalizers: %w", err)
		}
	}

	return nil
}

func (g *Group) ensureFinalizer(ctx context.Context, resource client.Object) error {
	if !controllerutil.ContainsFinalizer(resource, g.finalizer) {
		controllerutil.AddFinalizer(resource, g.finalizer)
		err := g.client.Update(ctx, resource)
		if err != nil {
			return errors.Errorf("failed to add finalizer: %w", err)
		}
	}

	return nil
}

func (g *Group) removeFinalizer(ctx context.Context, resource client.Object) error {
	logrus.Infof("removeFinalizer CALLED: Removing finalizer %s from resource %s/%s", g.finalizer, resource.GetNamespace(), resource.GetName())
	controllerutil.RemoveFinalizer(resource, g.finalizer)
	logrus.Infof("removeFinalizer: Calling client.Update to persist finalizer removal for %s/%s", resource.GetNamespace(), resource.GetName())
	err := g.client.Update(ctx, resource)
	if err != nil {
		logrus.WithError(err).Errorf("removeFinalizer FAILED: Could not update resource %s/%s", resource.GetNamespace(), resource.GetName())
		return errors.Errorf("failed to remove finalizer: %w", err)
	}

	logrus.Infof("removeFinalizer SUCCESS: Finalizer removed from %s/%s", resource.GetNamespace(), resource.GetName())
	return nil
}

func (g *Group) runGroup(ctx context.Context, req ctrl.Request, finalErr error, finalRes ctrl.Result) (ctrl.Result, error) {
	for _, reconciler := range g.reconcilers {
		logrus.Debugf("Starting cycle for %T", reconciler)
		res, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			if finalErr == nil {
				finalErr = err
			} else {
				logrus.WithError(err).Errorf("Error during reconciliation cycle for %T", reconciler)
			}
		}
		if !res.IsZero() {
			finalRes = shortestRequeue(res, finalRes)
		}
	}
	return finalRes, finalErr
}

func (g *Group) InjectRecorder(recorder record.EventRecorder) {
	g.recorder = recorder
	for _, reconciler := range g.reconcilers {
		reconciler.InjectRecorder(recorder)
	}
}

func isKubernetesRaceRelatedError(err error) bool {
	if k8sErr := &(k8serrors.StatusError{}); errors.As(err, &k8sErr) {
		return k8serrors.IsConflict(k8sErr) || k8serrors.IsNotFound(k8sErr) || k8serrors.IsForbidden(k8sErr) || k8serrors.IsAlreadyExists(k8sErr)
	}

	return false
}

func shortestRequeue(a, b reconcile.Result) reconcile.Result {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if a.RequeueAfter < b.RequeueAfter {
		return a
	}
	return b
}
