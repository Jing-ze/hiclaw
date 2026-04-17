package controller

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileInfrastructure ensures Matrix account, Gateway consumer, MinIO user,
// Matrix room, and credentials are provisioned. Idempotent: if already
// provisioned (MatrixUserID set), it refreshes credentials from the persisted store.
//
// The provision request derives Role and TeamRef from spec directly;
// TeamLeaderName is a soft observation from the Team CR (may be empty if
// the team's leader has not yet become ready, in which case infrastructure
// proceeds with a temporarily weaker Matrix Room power-levels set — a
// later reconcile will pick up the leader identity through the TeamRef
// watch and re-run ProvisionWorker for idempotent refinement).
func (r *WorkerReconciler) reconcileInfrastructure(ctx context.Context, s *workerScope) (reconcile.Result, error) {
	w := s.worker

	if w.Status.MatrixUserID != "" {
		refreshResult, err := r.Provisioner.RefreshCredentials(ctx, w.Name)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("refresh credentials: %w", err)
		}
		s.provResult = &service.WorkerProvisionResult{
			MatrixUserID:   w.Status.MatrixUserID,
			MatrixToken:    refreshResult.MatrixToken,
			RoomID:         w.Status.RoomID,
			GatewayKey:     refreshResult.GatewayKey,
			MinIOPassword:  refreshResult.MinIOPassword,
			MatrixPassword: refreshResult.MatrixPassword,
		}
		setCondition(&w.Status.Conditions, v1beta1.ConditionProvisioned, metav1.ConditionTrue,
			"CredentialsRefreshed", "", w.Generation)
		return reconcile.Result{}, nil
	}

	logger := log.FromContext(ctx)
	logger.Info("provisioning worker infrastructure", "name", w.Name)

	// Resolve the leader Matrix ID for Room power-levels at create time.
	// For team workers, we read the current leader from the Team CR;
	// if the leader is not yet observed, ProvisionWorker will fall back
	// to the manager as the authority (this is refined on the next
	// reconcile via the Team watch).
	teamLeaderName := resolveTeamLeaderName(ctx, r.Client, w)

	provResult, err := r.Provisioner.ProvisionWorker(ctx, service.WorkerProvisionRequest{
		Name:           w.Name,
		Role:           w.Spec.EffectiveRole(),
		TeamName:       w.Spec.TeamRef,
		TeamLeaderName: teamLeaderName,
		McpServers:     w.Spec.McpServers,
	})
	if err != nil {
		setCondition(&w.Status.Conditions, v1beta1.ConditionProvisioned, metav1.ConditionFalse,
			"ProvisionFailed", err.Error(), w.Generation)
		return reconcile.Result{}, fmt.Errorf("provision worker: %w", err)
	}

	w.Status.MatrixUserID = provResult.MatrixUserID
	w.Status.RoomID = provResult.RoomID
	s.provResult = provResult
	setCondition(&w.Status.Conditions, v1beta1.ConditionProvisioned, metav1.ConditionTrue,
		"Provisioned", "", w.Generation)

	return reconcile.Result{}, nil
}

// resolveTeamLeaderName looks up the leader Worker name for a team-scoped
// worker at infra provisioning time. Returns empty for standalone workers
// or when no leader is observable yet; provisioning will use the manager
// as the fallback authority in that case.
func resolveTeamLeaderName(ctx context.Context, c client.Client, w *v1beta1.Worker) string {
	if w.Spec.TeamRef == "" || w.Spec.EffectiveRole() != v1beta1.WorkerRoleTeamWorker {
		return ""
	}
	var team v1beta1.Team
	if err := c.Get(ctx, client.ObjectKey{Name: w.Spec.TeamRef, Namespace: w.Namespace}, &team); err != nil {
		return ""
	}
	if team.Status.Leader == nil {
		return ""
	}
	return team.Status.Leader.Name
}
