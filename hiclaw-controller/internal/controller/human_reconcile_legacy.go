package controller

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// reconcileHumanLegacy writes a humans-registry.json entry for backward
// compatibility with older Manager agent skills that still consult it.
// Legacy PermissionLevel and AccessibleTeams fields are synthesised from
// the new spec so the registry shape stays stable until Stage 11 reshapes
// it. Non-critical: Legacy nil / disabled short-circuits; errors logged.
func (r *HumanReconciler) reconcileHumanLegacy(ctx context.Context, s *humanScope) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	logger := log.FromContext(ctx)
	h := s.human

	entry := service.HumanRegistryEntry{
		Name:            h.Name,
		MatrixUserID:    h.Status.MatrixUserID,
		DisplayName:     h.Spec.DisplayName,
		PermissionLevel: syntheticPermissionLevel(h.Spec),
		AccessibleTeams: syntheticAccessibleTeams(h.Spec),
	}
	if err := r.Legacy.UpdateHumansRegistry(entry); err != nil {
		logger.Error(err, "humans-registry update failed (non-fatal)", "human", h.Name)
	}
}

// syntheticPermissionLevel maps the new access declarations back to the
// legacy 1/2/3 scheme used by the registry JSON.
//
//   - superAdmin         → 1 (full access)
//   - any admin teamAccess → 2 (team-scoped)
//   - any teamAccess / workerAccess → 2 (team-scoped; kept coarse-grained
//     to avoid a new level; workerAccess is represented out-of-band)
//   - empty spec         → 3 (worker-level default)
func syntheticPermissionLevel(spec v1beta1.HumanSpec) int {
	if spec.SuperAdmin {
		return 1
	}
	if len(spec.TeamAccess) > 0 {
		return 2
	}
	if len(spec.WorkerAccess) > 0 {
		return 3
	}
	return 3
}

// syntheticAccessibleTeams returns the list of team names the Human
// declares teamAccess for, regardless of admin vs member role.
func syntheticAccessibleTeams(spec v1beta1.HumanSpec) []string {
	if len(spec.TeamAccess) == 0 {
		return nil
	}
	out := make([]string, 0, len(spec.TeamAccess))
	for _, entry := range spec.TeamAccess {
		out = append(out, entry.Team)
	}
	return out
}
