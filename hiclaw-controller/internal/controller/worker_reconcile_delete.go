package controller

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *WorkerReconciler) reconcileDelete(ctx context.Context, s *workerScope) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	w := s.worker
	logger.Info("deleting worker", "name", w.Name)

	workerName := w.Name
	role := w.Spec.EffectiveRole()
	isTeamMember := role == v1beta1.WorkerRoleTeamLeader || role == v1beta1.WorkerRoleTeamWorker

	if err := r.Provisioner.DeactivateMatrixUser(ctx, workerName); err != nil {
		logger.Error(err, "matrix user deactivation failed (non-fatal)")
	}

	if err := r.Provisioner.DeprovisionWorker(ctx, service.WorkerDeprovisionRequest{
		Name:         workerName,
		IsTeamWorker: isTeamMember,
		McpServers:   w.Spec.McpServers,
		ExposedPorts: w.Status.ExposedPorts,
		ExposeSpec:   w.Spec.Expose,
	}); err != nil {
		logger.Error(err, "deprovision failed (non-fatal)")
	}

	if r.Backend != nil {
		if wb := r.Backend.DetectWorkerBackend(ctx); wb != nil {
			if err := wb.Delete(ctx, workerName); err != nil {
				logger.Error(err, "failed to delete worker container (may already be removed)")
			}
		}
	}

	if r.Legacy != nil && r.Legacy.Enabled() {
		// When the worker was a direct Manager peer (standalone or team
		// leader), remove it from Manager.groupAllowFrom so the Manager
		// agent config reflects current membership.
		if !isTeamMember || role == v1beta1.WorkerRoleTeamLeader {
			workerMatrixID := r.Provisioner.MatrixUserID(workerName)
			if err := r.Legacy.UpdateManagerGroupAllowFrom(workerMatrixID, false); err != nil {
				logger.Error(err, "failed to update Manager groupAllowFrom (non-fatal)")
			}
		}
		if err := r.Legacy.RemoveFromWorkersRegistry(workerName); err != nil {
			logger.Error(err, "failed to remove from workers registry (non-fatal)")
		}
	}

	if err := r.Deployer.CleanupOSSData(ctx, workerName); err != nil {
		logger.Error(err, "failed to clean up OSS agent data (non-fatal)")
	}
	if err := r.Provisioner.DeleteCredentials(ctx, workerName); err != nil {
		logger.Error(err, "failed to delete credentials (non-fatal)")
	}
	if err := r.Provisioner.DeleteServiceAccount(ctx, workerName); err != nil {
		logger.Error(err, "failed to delete ServiceAccount (non-fatal)")
	}

	controllerutil.RemoveFinalizer(w, finalizerName)
	if err := r.Update(ctx, w); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("worker deleted", "name", workerName)
	return reconcile.Result{}, nil
}
