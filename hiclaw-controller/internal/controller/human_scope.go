package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// humanScope carries per-reconcile state through the HumanReconciler phases.
// Populated incrementally by each phase; never retained across reconciles.
type humanScope struct {
	human     *v1beta1.Human
	patchBase client.Patch

	// Populated by reconcileHumanInfrastructure.
	//
	// matrixAccessToken is the user's own Matrix access token obtained from
	// EnsureUser / Login this cycle. Required by JoinRoom / LeaveRoom which
	// operate on behalf of the user. Not persisted — re-fetched each
	// reconcile via EnsureUser using the persisted password seed.
	matrixAccessToken string

	// Populated by reconcileHumanRooms.
	desiredRooms []string
}
