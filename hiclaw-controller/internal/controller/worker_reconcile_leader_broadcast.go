package controller

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileLeaderBroadcast writes the team coordination context into the
// leader worker's AGENTS.md on MinIO. This phase runs only when the
// worker is a team_leader AND the referenced Team exists (team found).
//
// The coordination context informs the leader of:
//   - its team name and Team Room ID for message routing
//   - its own Leader DM Room ID (the channel Manager uses)
//   - the heartbeat interval and worker-idle-timeout policy configured
//     at team level
//   - the list of team member worker names (for task delegation)
//   - the Matrix IDs of team admins (Humans with teamAccess role=admin)
//
// Invocation is idempotent: the deployer performs a read-modify-write on
// the existing AGENTS.md so repeated calls converge to the same content.
// A transient failure here does not abort reconcile — the worker is
// functional; only its awareness of team context is delayed.
func (r *WorkerReconciler) reconcileLeaderBroadcast(ctx context.Context, s *workerScope) (reconcile.Result, error) {
	w := s.worker

	if w.Spec.EffectiveRole() != v1beta1.WorkerRoleTeamLeader {
		return reconcile.Result{}, nil
	}
	if !s.teamFound {
		return reconcile.Result{}, nil
	}
	if s.teamRoomID == "" || s.teamLeaderDMRoomID == "" {
		log.FromContext(ctx).V(1).Info("deferring leader broadcast until team rooms are ready",
			"worker", w.Name, "team", s.teamName)
		return reconcile.Result{}, nil
	}

	// Pull heartbeat / idle timeout from the Team CR's Spec section. We
	// re-read the Team here (small cost; client cache served) to avoid
	// propagating these two fields through workerScope for a single
	// consumer. This keeps the scope focused on observation output.
	var team v1beta1.Team
	if err := r.Get(ctx, teamKey(w.Spec.TeamRef, w.Namespace), &team); err != nil {
		return reconcile.Result{}, fmt.Errorf("get team for leader broadcast: %w", err)
	}

	every := ""
	if team.Spec.Heartbeat != nil {
		every = team.Spec.Heartbeat.Every
	}

	if err := r.Deployer.WriteLeaderCoordinationContext(ctx, service.LeaderCoordinationRequest{
		LeaderName:         w.Name,
		TeamName:           s.teamName,
		TeamRoomID:         s.teamRoomID,
		LeaderDMRoomID:     s.teamLeaderDMRoomID,
		HeartbeatEvery:     every,
		WorkerIdleTimeout:  team.Spec.WorkerIdleTimeout,
		TeamMemberNames:    s.teamMemberNames,
		TeamAdminMatrixIDs: s.teamAdminMatrixIDs,
	}); err != nil {
		// Non-fatal: log and continue. Next reconcile retries; Team CR
		// changes (through Watches) will naturally requeue.
		log.FromContext(ctx).Error(err, "leader coordination broadcast failed (non-fatal)",
			"worker", w.Name, "team", s.teamName)
	}
	return reconcile.Result{}, nil
}
