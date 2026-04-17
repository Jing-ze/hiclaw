package controller

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileConfig ensures all configuration (package, inline configs, openclaw.json,
// SOUL.md, mcporter config, AGENTS.md, builtin skills) is deployed to OSS.
// Idempotent: safe to re-run; OSS writes overwrite existing files.
//
// Spec-driven inputs:
//   - Role / TeamRef come from Worker.spec (no longer from annotations).
//   - Effective ChannelPolicy, team leader name, and a single representative
//     team admin Matrix ID come from workerScope (populated by
//     reconcileTeamMembership via Team.status observation).
func (r *WorkerReconciler) reconcileConfig(ctx context.Context, s *workerScope) (reconcile.Result, error) {
	if s.provResult == nil {
		return reconcile.Result{}, nil
	}

	w := s.worker
	logger := log.FromContext(ctx)
	workerName := w.Name
	role := w.Spec.EffectiveRole()
	consumerName := "worker-" + workerName

	// First admin (if any) is plumbed into the per-worker agent config
	// for backwards compatibility with the existing agentconfig.CoordinationContext
	// schema which holds a single TeamAdminID. Full multi-admin support
	// lives in reconcileLeaderBroadcast for leader workers.
	var teamAdminMatrixID string
	if len(s.teamAdminMatrixIDs) > 0 {
		teamAdminMatrixID = s.teamAdminMatrixIDs[0]
	}

	isUpdate := w.Status.Phase != "" && w.Status.Phase != "Pending" && w.Status.Phase != "Failed"

	if err := r.Deployer.DeployPackage(ctx, workerName, w.Spec.Package, isUpdate); err != nil {
		return reconcile.Result{}, fmt.Errorf("deploy package: %w", err)
	}
	// WriteInlineConfigs consumes Worker.spec directly but we re-shape the
	// spec to route through the effective ChannelPolicy for consistency
	// with the config deploy call below.
	if err := r.Deployer.WriteInlineConfigs(workerName, w.Spec); err != nil {
		return reconcile.Result{}, fmt.Errorf("write inline configs: %w", err)
	}

	var authorizedMCPs []string
	if isUpdate && len(w.Spec.McpServers) > 0 {
		var err error
		authorizedMCPs, err = r.Provisioner.ReconcileMCPAuth(ctx, consumerName, w.Spec.McpServers)
		if err != nil {
			logger.Error(err, "MCP reauthorization failed (non-fatal)")
		}
	} else {
		authorizedMCPs = s.provResult.AuthorizedMCPs
	}

	// Compose the spec handed to Deployer: keep the original user spec but
	// swap in the effective ChannelPolicy so the generated openclaw.json
	// reflects team-merged permissions.
	deploySpec := w.Spec
	if s.effectivePolicy != nil {
		deploySpec.ChannelPolicy = s.effectivePolicy
	}

	if err := r.Deployer.DeployWorkerConfig(ctx, service.WorkerDeployRequest{
		Name:              workerName,
		Spec:              deploySpec,
		Role:              role,
		TeamName:          s.teamName,
		TeamLeaderName:    s.teamLeaderName,
		MatrixToken:       s.provResult.MatrixToken,
		GatewayKey:        s.provResult.GatewayKey,
		MatrixPassword:    s.provResult.MatrixPassword,
		AuthorizedMCPs:    authorizedMCPs,
		TeamAdminMatrixID: teamAdminMatrixID,
		IsUpdate:          isUpdate,
	}); err != nil {
		return reconcile.Result{}, fmt.Errorf("deploy worker config: %w", err)
	}

	if err := r.Deployer.PushOnDemandSkills(ctx, workerName, w.Spec.Skills); err != nil {
		logger.Error(err, "skill push failed (non-fatal)")
	}

	return reconcile.Result{}, nil
}

// Compile-time reference to avoid "declared and not used" if the file
// happens to shrink — keeps the import footprint meaningful.
var _ = v1beta1.WorkerRoleStandalone
