package controller

import (
	"context"
	"fmt"

	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileHumanInfrastructure ensures the Human's Matrix account exists
// and fetches an access token for subsequent room operations.
//
// Credential handling: status.initialPassword doubles as the persisted
// password seed (there is no separate credential store for Humans).
// First reconcile: EnsureUser with a generated password; the returned
// password is written into status.initialPassword (shown once at create
// time) AND reused on subsequent reconciles so Login succeeds against
// the existing Matrix user. MatrixUserID is set once.
func (r *HumanReconciler) reconcileHumanInfrastructure(ctx context.Context, s *humanScope) (reconcile.Result, error) {
	h := s.human

	req := matrix.EnsureUserRequest{Username: h.Name}
	if h.Status.InitialPassword != "" {
		req.Password = h.Status.InitialPassword
	}

	creds, err := r.Matrix.EnsureUser(ctx, req)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("matrix ensure user: %w", err)
	}

	s.matrixAccessToken = creds.AccessToken

	if h.Status.MatrixUserID == "" {
		h.Status.MatrixUserID = r.Matrix.UserID(h.Name)
	}
	if h.Status.InitialPassword == "" {
		h.Status.InitialPassword = creds.Password
	}
	return reconcile.Result{}, nil
}
