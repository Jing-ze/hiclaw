package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
)

// computeHumanPhase collapses Human status into a single high-level phase.
// Active: Matrix account provisioned and desired rooms converged.
// Pending: Matrix account present but rooms not yet joined (or infra
// not yet run this reconcile).
// Failed: infra-level failure (no MatrixUserID yet and reconcileErr set).
func computeHumanPhase(h *v1beta1.Human, reconcileErr error) string {
	if reconcileErr != nil {
		if h.Status.MatrixUserID == "" {
			return "Failed"
		}
		if h.Status.Phase == "" {
			return "Pending"
		}
		return h.Status.Phase
	}
	if h.Status.MatrixUserID == "" {
		return "Pending"
	}
	return "Active"
}
