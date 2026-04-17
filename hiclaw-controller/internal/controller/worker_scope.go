package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// workerScope carries per-reconcile state through the WorkerReconciler
// phases. Populated incrementally; never retained across reconciles.
type workerScope struct {
	worker     *v1beta1.Worker
	provResult *service.WorkerProvisionResult
	patchBase  client.Patch

	// --- Team context, populated by reconcileTeamMembership ---
	//
	// teamFound is true when spec.teamRef resolved to an existing Team CR.
	// When false (teamRef missing or Team not found), leader broadcast and
	// team-scoped config merging are skipped; the worker still runs with
	// a standalone-style effective policy.
	teamFound bool

	// teamName mirrors spec.teamRef after resolution (empty when no team
	// context). Plumbed into service.WorkerDeployRequest.TeamName and
	// service.WorkerProvisionRequest.TeamName.
	teamName string

	// teamLeaderName is the name of the resolved team_leader Worker
	// observed via Team.status.leader. Empty when the team has no leader
	// or when the worker itself is the leader.
	teamLeaderName string

	// teamLeaderMatrixID is the Matrix ID of the resolved leader;
	// populated only when the leader is ready.
	teamLeaderMatrixID string

	// teamLeaderDMRoomID / teamRoomID are the observed Matrix Room IDs
	// pulled from Team.status. Passed to WriteLeaderCoordinationContext
	// when this worker is the leader.
	teamLeaderDMRoomID string
	teamRoomID         string

	// teamMemberNames is the list of team_worker names in the same team
	// (excluding this worker when it is the leader). Used by
	// WriteLeaderCoordinationContext to populate the workers list.
	teamMemberNames []string

	// teamMemberMatrixIDs is the Matrix ID list of team_worker peers
	// (excluding the worker itself). Used when composing effective
	// groupAllowFrom with peerMentions enabled.
	teamMemberMatrixIDs []string

	// teamAdminMatrixIDs is the Matrix ID list of Humans with
	// teamAccess[].role=admin targeting this team.
	teamAdminMatrixIDs []string

	// peerMentionsEnabled mirrors Team.spec.peerMentions (default true).
	peerMentionsEnabled bool

	// effectivePolicy is the merged ChannelPolicySpec applied by the
	// config phase: team-scope defaults + worker overrides + computed
	// peer / admin / leader additions derived from team observations.
	effectivePolicy *v1beta1.ChannelPolicySpec
}

// computePhase determines the Worker status phase based on reconcile outcome.
// When reconcile succeeds, phase reflects the desired lifecycle state.
// When reconcile fails, phase depends on whether infrastructure was provisioned.
func computePhase(w *v1beta1.Worker, reconcileErr error) string {
	if reconcileErr != nil {
		if w.Status.MatrixUserID == "" {
			return "Failed"
		}
		if w.Status.Phase == "" {
			return "Pending"
		}
		return w.Status.Phase
	}
	return w.Spec.DesiredState()
}
