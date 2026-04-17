package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileHumanDelete is the finalizer path. It deactivates the Human's
// Matrix account, removes the legacy registry entry, and releases the
// finalizer. Matrix room memberships held by the user are left in place
// — deactivation prevents login without needing to explicitly leave
// every room.
func (r *HumanReconciler) reconcileHumanDelete(ctx context.Context, s *humanScope) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	h := s.human
	logger.Info("deleting human", "name", h.Name)

	if err := r.Matrix.DeactivateUser(ctx, h.Name); err != nil {
		logger.Error(err, "matrix user deactivation failed (non-fatal)", "human", h.Name)
	}

	if r.Legacy != nil && r.Legacy.Enabled() {
		if err := r.Legacy.RemoveFromHumansRegistry(ctx, h.Name); err != nil {
			logger.Error(err, "humans-registry remove failed (non-fatal)", "human", h.Name)
		}
	}

	controllerutil.RemoveFinalizer(h, finalizerName)
	if err := r.Update(ctx, h); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("human deleted", "name", h.Name)
	return reconcile.Result{}, nil
}
