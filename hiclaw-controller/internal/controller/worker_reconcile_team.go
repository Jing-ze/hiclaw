package controller

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileTeamMembership resolves the worker's team context from the
// referenced Team CR and computes the effective ChannelPolicySpec.
//
// When spec.teamRef is empty (standalone worker), the phase writes
// TeamRefResolved=True (trivially resolved), clears any previously
// observed teamRef, and moves on with only the per-worker policy.
//
// When spec.teamRef is set but the Team CR does not exist yet, the phase
// writes TeamRefResolved=False with reason=TeamNotFound and proceeds
// with a degraded context (no team peers, no admin broadcast). The
// worker's own infrastructure (Matrix / Pod) is unaffected; only the
// team-scoped coordination is deferred until the Team resource appears.
//
// When spec.teamRef resolves, the phase populates workerScope with the
// observed leader, member, and admin sets from Team.status, merges
// Team.spec.channelPolicy with Worker.spec.channelPolicy, and adds the
// automatic peer / admin / leader entries derived from the observation.
// This effective policy is then consumed by reconcileConfig.
func (r *WorkerReconciler) reconcileTeamMembership(ctx context.Context, s *workerScope) (reconcile.Result, error) {
	w := s.worker
	logger := log.FromContext(ctx)

	if w.Spec.TeamRef == "" {
		setCondition(&w.Status.Conditions, v1beta1.ConditionTeamRefResolved, metav1.ConditionTrue,
			"Standalone", "worker has no teamRef", w.Generation)
		if w.Status.TeamRef != "" {
			logger.Info("worker left team", "worker", w.Name, "previousTeam", w.Status.TeamRef)
		}
		w.Status.TeamRef = ""
		s.effectivePolicy = cloneChannelPolicy(w.Spec.ChannelPolicy)
		return reconcile.Result{}, nil
	}

	var team v1beta1.Team
	if err := r.Get(ctx, client.ObjectKey{Name: w.Spec.TeamRef, Namespace: w.Namespace}, &team); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, fmt.Errorf("get team %q: %w", w.Spec.TeamRef, err)
		}
		setCondition(&w.Status.Conditions, v1beta1.ConditionTeamRefResolved, metav1.ConditionFalse,
			"TeamNotFound",
			fmt.Sprintf("referenced team %q does not exist", w.Spec.TeamRef),
			w.Generation)
		s.teamFound = false
		s.teamName = w.Spec.TeamRef
		s.effectivePolicy = cloneChannelPolicy(w.Spec.ChannelPolicy)
		logger.V(1).Info("team not found, running with degraded context",
			"worker", w.Name, "teamRef", w.Spec.TeamRef)
		return reconcile.Result{}, nil
	}

	s.teamFound = true
	s.teamName = team.Name
	s.peerMentionsEnabled = effectivePeerMentions(team.Spec)

	// Observe leader from Team.status (may be nil if the team has no leader
	// or if this worker IS the leader; both cases are normal).
	if team.Status.Leader != nil && team.Status.Leader.Name != w.Name {
		s.teamLeaderName = team.Status.Leader.Name
		s.teamLeaderMatrixID = team.Status.Leader.MatrixUserID
	}
	s.teamRoomID = team.Status.TeamRoomID
	s.teamLeaderDMRoomID = team.Status.LeaderDMRoomID

	// Observe team members (team_workers). Exclude self from peer lists.
	for _, m := range team.Status.Members {
		if m.Name == w.Name {
			continue
		}
		s.teamMemberNames = append(s.teamMemberNames, m.Name)
		if m.MatrixUserID != "" {
			s.teamMemberMatrixIDs = append(s.teamMemberMatrixIDs, m.MatrixUserID)
		}
	}

	// Observe team admins.
	for _, a := range team.Status.Admins {
		if a.MatrixUserID != "" {
			s.teamAdminMatrixIDs = append(s.teamAdminMatrixIDs, a.MatrixUserID)
		}
	}

	// Detect cross-team migration for observability only; migration mechanics
	// (Matrix room membership changes) are driven by Team reconciler via
	// ReconcileTeamRoomMembership rather than by this phase.
	if w.Status.TeamRef != "" && w.Status.TeamRef != w.Spec.TeamRef {
		logger.Info("worker migrating between teams",
			"worker", w.Name, "from", w.Status.TeamRef, "to", w.Spec.TeamRef)
	}
	w.Status.TeamRef = w.Spec.TeamRef

	s.effectivePolicy = buildEffectivePolicy(team, w, s)
	setCondition(&w.Status.Conditions, v1beta1.ConditionTeamRefResolved, metav1.ConditionTrue,
		"TeamFound", fmt.Sprintf("member of team %q", team.Name), w.Generation)
	return reconcile.Result{}, nil
}

// buildEffectivePolicy produces the ChannelPolicySpec applied to the
// worker's agent config. Layering order (lowest to highest precedence):
//
//  1. Team.spec.channelPolicy (team-wide defaults)
//  2. Worker.spec.channelPolicy (per-worker overrides)
//  3. Automatic allow-from additions derived from team observation:
//     - team_worker: +leader, +team admins, +peers when peerMentions
//     - team_leader: +team members, +team admins
//
// Deny rules from (1) and (2) are preserved; automatic additions only
// contribute to groupAllowExtra, never to deny lists.
func buildEffectivePolicy(team v1beta1.Team, w *v1beta1.Worker, s *workerScope) *v1beta1.ChannelPolicySpec {
	merged := mergeChannelPolicy(team.Spec.ChannelPolicy, w.Spec.ChannelPolicy)
	if merged == nil {
		merged = &v1beta1.ChannelPolicySpec{}
	}

	role := w.Spec.EffectiveRole()
	switch role {
	case v1beta1.WorkerRoleTeamWorker:
		if s.teamLeaderMatrixID != "" {
			merged.GroupAllowExtra = appendUnique(merged.GroupAllowExtra, s.teamLeaderMatrixID)
		}
		for _, admin := range s.teamAdminMatrixIDs {
			merged.GroupAllowExtra = appendUnique(merged.GroupAllowExtra, admin)
		}
		if s.peerMentionsEnabled {
			for _, peer := range s.teamMemberMatrixIDs {
				merged.GroupAllowExtra = appendUnique(merged.GroupAllowExtra, peer)
			}
		}
	case v1beta1.WorkerRoleTeamLeader:
		for _, m := range s.teamMemberMatrixIDs {
			merged.GroupAllowExtra = appendUnique(merged.GroupAllowExtra, m)
		}
		for _, admin := range s.teamAdminMatrixIDs {
			merged.GroupAllowExtra = appendUnique(merged.GroupAllowExtra, admin)
		}
	}
	return merged
}

// mergeChannelPolicy concatenates two nullable ChannelPolicySpec values.
// Later (right) arguments are appended after earlier (left) entries so
// the effective order is team-defaults → worker-overrides. Does not
// deduplicate — callers should treat the result as a multi-set until
// appendUnique is applied.
func mergeChannelPolicy(a, b *v1beta1.ChannelPolicySpec) *v1beta1.ChannelPolicySpec {
	if a == nil && b == nil {
		return nil
	}
	out := &v1beta1.ChannelPolicySpec{}
	if a != nil {
		out.GroupAllowExtra = append(out.GroupAllowExtra, a.GroupAllowExtra...)
		out.GroupDenyExtra = append(out.GroupDenyExtra, a.GroupDenyExtra...)
		out.DmAllowExtra = append(out.DmAllowExtra, a.DmAllowExtra...)
		out.DmDenyExtra = append(out.DmDenyExtra, a.DmDenyExtra...)
	}
	if b != nil {
		out.GroupAllowExtra = append(out.GroupAllowExtra, b.GroupAllowExtra...)
		out.GroupDenyExtra = append(out.GroupDenyExtra, b.GroupDenyExtra...)
		out.DmAllowExtra = append(out.DmAllowExtra, b.DmAllowExtra...)
		out.DmDenyExtra = append(out.DmDenyExtra, b.DmDenyExtra...)
	}
	return out
}

// cloneChannelPolicy returns a deep copy of the spec or nil.
func cloneChannelPolicy(p *v1beta1.ChannelPolicySpec) *v1beta1.ChannelPolicySpec {
	if p == nil {
		return nil
	}
	out := p.DeepCopy()
	return out
}

// appendUnique adds v to the slice only when it is non-empty and not
// already present. Preserves insertion order.
func appendUnique(slice []string, v string) []string {
	if v == "" {
		return slice
	}
	for _, existing := range slice {
		if existing == v {
			return slice
		}
	}
	return append(slice, v)
}
