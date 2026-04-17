//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// Manager Create tests
// ---------------------------------------------------------------------------

func TestManagerCreate_HappyPath(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-create")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.ObservedGeneration != m.Generation {
		t.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
	}
	if m.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set after creation")
	}
	if m.Status.RoomID == "" {
		t.Error("RoomID should be set after creation")
	}
	provCount, _, _, _ := mockMgrProv.CallCounts()
	if provCount == 0 {
		t.Error("ProvisionManager should have been called")
	}
	_, deployConfigCount, _, _ := mockMgrDeploy.CallCounts()
	if deployConfigCount == 0 {
		t.Error("DeployManagerConfig should have been called")
	}
}

func TestManagerCreate_ProvisionFailure_SetsFailedPhase(t *testing.T) {
	resetManagerMocks()

	mockMgrProv.ProvisionManagerFn = func(_ context.Context, _ service.ManagerProvisionRequest) (*service.ManagerProvisionResult, error) {
		return nil, fmt.Errorf("simulated provision failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-fail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", m.Status.Phase)
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Manager Delete tests
// ---------------------------------------------------------------------------

func TestManagerDelete_CleansUpAll(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-delete")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}

	waitForManagerRunning(t, mgr)

	mockMgrProv.ClearCalls()
	mockMgrDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, mgr); err != nil {
		t.Fatalf("failed to delete Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m)
		if err == nil {
			return fmt.Errorf("manager still exists (phase=%q)", m.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	_, deprovCount, _, deactivateCount := mockMgrProv.CallCounts()
	if deactivateCount == 0 {
		t.Error("DeactivateMatrixUser should have been called")
	}
	if deprovCount == 0 {
		t.Error("DeprovisionManager should have been called")
	}
	_, _, _, cleanupCount := mockMgrDeploy.CallCounts()
	if cleanupCount == 0 {
		t.Error("CleanupOSSData should have been called")
	}
}

// ---------------------------------------------------------------------------
// Manager Finalizer test
// ---------------------------------------------------------------------------

func TestManagerFinalizer_AddedOnCreate(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-fin")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		for _, f := range m.Finalizers {
			if f == "hiclaw.io/cleanup" {
				return nil
			}
		}
		return fmt.Errorf("finalizer hiclaw.io/cleanup not found in %v", m.Finalizers)
	})
}

// ---------------------------------------------------------------------------
// Manager Update test
// ---------------------------------------------------------------------------

func TestManagerUpdate_SpecChange_RecreatesContainer(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-update")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	mockMgrBackend.Reset()
	mockMgrBackend.StatusFn = func(_ context.Context, _ string) (*backend.WorkerResult, error) {
		return &backend.WorkerResult{Status: backend.StatusRunning}, nil
	}

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.Model = "claude-sonnet-4-20250514"
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.ObservedGeneration != m.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
		}
		return nil
	})

	creates, deletes, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(deletes) == 0 {
		t.Error("backend.Delete should have been called to remove old container")
	}
	if len(creates) == 0 {
		t.Error("backend.Create should have been called to create new container")
	}
}

// ---------------------------------------------------------------------------
// Manager Idempotency test
// ---------------------------------------------------------------------------

func TestManagerCreate_Idempotent_NoDoubleProvision(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-idemp")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	provCountBefore, _, refreshCountBefore, _ := mockMgrProv.CallCounts()

	triggerManagerReconcile(t, mgr)

	assertEventually(t, func() error {
		_, _, refreshCount, _ := mockMgrProv.CallCounts()
		if refreshCount <= refreshCountBefore {
			return fmt.Errorf("RefreshManagerCredentials count=%d, want >%d",
				refreshCount, refreshCountBefore)
		}
		return nil
	})

	provCountAfter, _, _, _ := mockMgrProv.CallCounts()
	if provCountAfter != provCountBefore {
		t.Errorf("ProvisionManager called %d times, want %d (should not re-provision after Running)",
			provCountAfter, provCountBefore)
	}
}

// ---------------------------------------------------------------------------
// Manager Lifecycle state change tests
// ---------------------------------------------------------------------------

func TestManagerStateChange_StopAndResume(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-stop")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// Running -> Stopped
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = ptrString("Stopped")
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Stopped" {
			return fmt.Errorf("phase=%q, want Stopped", m.Status.Phase)
		}
		return nil
	})

	_, deletes, _, stops, _ := mockMgrBackend.CallSnapshot()
	if len(stops)+len(deletes) == 0 {
		t.Error("backend.Stop or Delete should have been called when transitioning to Stopped")
	}

	// Stopped -> Running
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = nil
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when resuming from Stopped")
	}
}

// ---------------------------------------------------------------------------
// Manager Delete of failed manager
// ---------------------------------------------------------------------------

func TestManagerDelete_ProvisionFailed_StillCleans(t *testing.T) {
	resetManagerMocks()

	mockMgrProv.ProvisionManagerFn = func(_ context.Context, _ service.ManagerProvisionRequest) (*service.ManagerProvisionResult, error) {
		return nil, fmt.Errorf("simulated provision failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-delfail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Failed" {
			return fmt.Errorf("phase=%q, want Failed", m.Status.Phase)
		}
		return nil
	})

	mockMgrProv.ClearCalls()
	mockMgrDeploy.ClearCalls()

	if err := k8sClient.Delete(ctx, mgr); err != nil {
		t.Fatalf("failed to delete Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m)
		if err == nil {
			return fmt.Errorf("manager still exists (phase=%q)", m.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})

	_, deprovCount, _, _ := mockMgrProv.CallCounts()
	if deprovCount == 0 {
		t.Error("DeprovisionManager should have been called even for a failed manager")
	}
	_, _, _, cleanupCount := mockMgrDeploy.CallCounts()
	if cleanupCount == 0 {
		t.Error("CleanupOSSData should have been called even for a failed manager")
	}
}

// ---------------------------------------------------------------------------
// Manager no infinite recreate loop
// ---------------------------------------------------------------------------

func TestManagerUpdate_NoInfiniteRecreate(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-noloop")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.Model = "gpt-4o-mini"
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.ObservedGeneration != m.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
		}
		return nil
	})

	time.Sleep(3 * time.Second)

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("expected at least 1 Create from spec update")
	}
	if len(creates) > 2 {
		t.Errorf("Create called %d times -- possible infinite recreate loop (want <=2)", len(creates))
	}
}

// ---------------------------------------------------------------------------
// Manager Sleeping lifecycle test
// ---------------------------------------------------------------------------

func TestManagerStateChange_SleepAndWake(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-sleep")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// --- Running -> Sleeping ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = ptrString("Sleeping")
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Sleeping" {
			return fmt.Errorf("phase=%q, want Sleeping", m.Status.Phase)
		}
		return nil
	})

	_, _, _, stops, _ := mockMgrBackend.CallSnapshot()
	if len(stops) == 0 {
		t.Error("backend.Stop should have been called when transitioning to Sleeping")
	}

	// --- Sleeping -> Running ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = nil
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when waking from Sleeping")
	}
}

// ---------------------------------------------------------------------------
// Manager Pod deleted recreates test
// ---------------------------------------------------------------------------

func TestManagerPodDeleted_Recreates(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-poddel")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// Simulate external pod deletion via the mock's automatic state tracking.
	// The ContainerName alias (Issue #1 fix) means "hiclaw-manager" is tracked
	// alongside req.Name, so SimulatePodDeletion works for Manager now.
	containerName := managerContainerName(mgrName)
	mockMgrBackend.SimulatePodDeletion(containerName)
	mockMgrBackend.ClearCalls()

	triggerManagerReconcile(t, mgr)

	assertEventually(t, func() error {
		creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
		if len(creates) == 0 {
			return fmt.Errorf("waiting for backend.Create to be called (pod recreation)")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}
	if m.Status.Phase != "Running" {
		t.Errorf("phase=%q after pod recreation, want Running", m.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Manager simultaneous spec + state change test
// ---------------------------------------------------------------------------

func TestManagerStateChange_SimultaneousSpecAndState(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-simul")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// --- Simultaneously change state to Stopped AND model ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = ptrString("Stopped")
		m.Spec.Model = "claude-sonnet-4-20250514"
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Stopped" {
			return fmt.Errorf("phase=%q, want Stopped", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ := mockMgrBackend.CallSnapshot()
	if len(creates) > 0 {
		t.Errorf("backend.Create called %d times while Stopped -- should not create in Stopped state", len(creates))
	}

	// --- Resume to Running with new config ---
	mockMgrBackend.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.State = nil
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q, want Running", m.Status.Phase)
		}
		return nil
	})

	creates, _, _, _, _ = mockMgrBackend.CallSnapshot()
	if len(creates) == 0 {
		t.Error("backend.Create should have been called when resuming with new config")
	}
}

// ---------------------------------------------------------------------------
// Manager error path tests
// ---------------------------------------------------------------------------

func TestManagerCreate_ConfigDeployFailure_KeepsPhase(t *testing.T) {
	resetManagerMocks()

	mockMgrDeploy.DeployManagerConfigFn = func(_ context.Context, _ service.ManagerDeployRequest) error {
		return fmt.Errorf("simulated config deploy failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-cfgfail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set (provision succeeded before config failure)")
	}
	if m.Status.Phase != "Pending" {
		t.Errorf("Phase=%q, want Pending (infra provisioned but config failed)", m.Status.Phase)
	}
	if m.Status.ObservedGeneration != 0 {
		t.Errorf("ObservedGeneration=%d, want 0 (should not be written on error)", m.Status.ObservedGeneration)
	}
}

func TestManagerCreate_ContainerCreateFailure_ReturnsError(t *testing.T) {
	resetManagerMocks()

	mockMgrBackend.CreateFn = func(_ context.Context, _ backend.CreateRequest) (*backend.WorkerResult, error) {
		return nil, fmt.Errorf("simulated container create failure")
	}

	mgrName := fixtures.UniqueName("test-mgr-ctrfail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain failure reason")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.MatrixUserID == "" {
		t.Error("MatrixUserID should be set (infra+config succeeded before container failure)")
	}
	if m.Status.Phase == "Running" {
		t.Error("Phase should not be Running when container creation failed")
	}
}

func TestManagerCreate_ServiceAccountFailure_RetriesOnNextReconcile(t *testing.T) {
	resetManagerMocks()

	saCallCount := 0
	mockMgrProv.EnsureManagerServiceAccountFn = func(_ context.Context, _ string) error {
		saCallCount++
		if saCallCount <= 1 {
			return fmt.Errorf("simulated SA creation failure")
		}
		return nil
	}

	mgrName := fixtures.UniqueName("test-mgr-safail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	// SA fails on first reconcile, succeeds on retry → Manager reaches Running.
	waitForManagerRunning(t, mgr)

	ensureSA, _ := mockMgrProv.ServiceAccountCallCounts()
	if ensureSA < 2 {
		t.Errorf("EnsureManagerServiceAccount called %d times, want >=2 (initial failure + retry)", ensureSA)
	}
}

func TestManagerUpdate_RefreshCredentialsFail_KeepsPhase(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-reffail")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	// Switch RefreshManagerCredentials to fail
	mockMgrProv.RefreshManagerCredentialsFn = func(_ context.Context, _ string) (*service.RefreshResult, error) {
		return nil, fmt.Errorf("simulated refresh failure")
	}

	triggerManagerReconcile(t, mgr)

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Message == "" {
			return fmt.Errorf("message should contain refresh failure")
		}
		return nil
	})

	var m v1beta1.Manager
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
		t.Fatalf("failed to get Manager: %v", err)
	}

	if m.Status.Phase != "Running" {
		t.Errorf("Phase=%q, want Running (should keep original phase on refresh failure)", m.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// Manager delete resilience test
// ---------------------------------------------------------------------------

func TestManagerDelete_PartialFailure_StillCompletes(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-delprt")
	mgr := fixtures.NewTestManager(mgrName)

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}

	waitForManagerRunning(t, mgr)

	// Make cleanup operations fail
	mockMgrProv.DeprovisionManagerFn = func(_ context.Context, _ string, _ []string) error {
		return fmt.Errorf("simulated deprovision failure")
	}
	mockMgrDeploy.CleanupOSSDataFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("simulated OSS cleanup failure")
	}
	mockMgrProv.DeleteCredentialsFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("simulated credential delete failure")
	}

	if err := k8sClient.Delete(ctx, mgr); err != nil {
		t.Fatalf("failed to delete Manager CR: %v", err)
	}

	assertEventually(t, func() error {
		var m v1beta1.Manager
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m)
		if err == nil {
			return fmt.Errorf("manager still exists (phase=%q)", m.Status.Phase)
		}
		return client.IgnoreNotFound(err)
	})
}

// ---------------------------------------------------------------------------
// Manager MCP reauthorization test
// ---------------------------------------------------------------------------

func TestManagerUpdate_MCPServersChange_TriggersReauth(t *testing.T) {
	resetManagerMocks()

	mgrName := fixtures.UniqueName("test-mgr-mcp")
	mgr := fixtures.NewTestManagerWithMCPServers(mgrName, []string{"mcp-server-1"})

	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("failed to create Manager CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, mgr)
	})

	waitForManagerRunning(t, mgr)

	mockMgrProv.ClearCalls()

	updateManagerSpecField(t, mgr, func(m *v1beta1.Manager) {
		m.Spec.McpServers = []string{"mcp-server-1", "mcp-server-2"}
	})

	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.ObservedGeneration != m.Generation {
			return fmt.Errorf("ObservedGeneration=%d, want %d", m.Status.ObservedGeneration, m.Generation)
		}
		return nil
	})

	mcpCount := mockMgrProv.MCPAuthCallCount()
	if mcpCount == 0 {
		t.Error("ReconcileMCPAuth should have been called after McpServers change")
	}
}

// ---------------------------------------------------------------------------
// Stage 12 extensions: Manager allowFrom reactivity.
// ---------------------------------------------------------------------------

// TestManager_AllowFromReactsToWorkerChanges verifies that the Manager
// reconciler recomputes groupAllowFromExtra when Workers join / leave
// the authoritative list (standalone + team_leader Workers with a
// provisioned Matrix ID).
func TestManager_AllowFromReactsToWorkerChanges(t *testing.T) {
	resetAllMocks()

	mgrName := fixtures.UniqueName("m-af-w")
	standaloneName := fixtures.UniqueName("m-af-standalone")
	teamName := fixtures.UniqueName("m-af-team")
	leaderName := teamName + "-lead"

	mgr := fixtures.NewTestManager(mgrName)
	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mgr) })
	waitForManagerRunning(t, mgr)

	// Baseline: Workers/Humans don't exist yet, so allowFromExtra should be empty.
	mockMgrDeploy.ClearCalls()

	// Create a standalone Worker + a team_leader Worker (in a Team).
	standalone := fixtures.NewTestWorker(standaloneName)
	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	for _, obj := range []client.Object{standalone, team, leader} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, standalone)
		_ = k8sClient.Delete(ctx, leader)
		_ = k8sClient.Delete(ctx, team)
	})
	waitForRunning(t, standalone)
	waitForRunning(t, leader)

	standaloneMatrix := "@" + standaloneName + ":localhost"
	leaderMatrix := "@" + leaderName + ":localhost"

	// Wait until some DeployManagerConfig call carries both IDs.
	assertEventually(t, func() error {
		calls := mockMgrDeploy.Calls.DeployManagerConfig
		for i := len(calls) - 1; i >= 0; i-- {
			if containsString(calls[i].GroupAllowFromExtra, standaloneMatrix) &&
				containsString(calls[i].GroupAllowFromExtra, leaderMatrix) {
				return nil
			}
		}
		return fmt.Errorf("no DeployManagerConfig call with both %q and %q in GroupAllowFromExtra (calls=%d)",
			standaloneMatrix, leaderMatrix, len(calls))
	})

	// Demote the leader to team_worker; it should drop out of allowFromExtra.
	baseline := len(mockMgrDeploy.Calls.DeployManagerConfig)
	updateSpecField(t, leader, func(w *v1beta1.Worker) {
		w.Spec.Role = v1beta1.WorkerRoleTeamWorker
	})

	assertEventually(t, func() error {
		calls := mockMgrDeploy.Calls.DeployManagerConfig
		for i := len(calls) - 1; i >= baseline; i-- {
			hasLeader := containsString(calls[i].GroupAllowFromExtra, leaderMatrix)
			hasStandalone := containsString(calls[i].GroupAllowFromExtra, standaloneMatrix)
			if !hasLeader && hasStandalone {
				return nil
			}
		}
		return fmt.Errorf("no post-demotion DeployManagerConfig call with leader removed (baseline=%d calls=%d)",
			baseline, len(calls))
	})
}

// TestManager_AllowFromReactsToHumanChanges verifies Manager allowFrom
// picks up superAdmin Humans and drops them when the flag is cleared.
func TestManager_AllowFromReactsToHumanChanges(t *testing.T) {
	resetAllMocks()

	mgrName := fixtures.UniqueName("m-af-h")
	humanName := fixtures.UniqueName("m-af-super")

	mgr := fixtures.NewTestManager(mgrName)
	if err := k8sClient.Create(ctx, mgr); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, mgr) })
	waitForManagerRunning(t, mgr)

	mockMgrDeploy.ClearCalls()

	human := fixtures.NewTestHuman(humanName, fixtures.WithSuperAdmin())
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	humanMatrix := "@" + humanName + ":localhost"

	assertEventually(t, func() error {
		calls := mockMgrDeploy.Calls.DeployManagerConfig
		for i := len(calls) - 1; i >= 0; i-- {
			if containsString(calls[i].GroupAllowFromExtra, humanMatrix) {
				return nil
			}
		}
		return fmt.Errorf("no DeployManagerConfig call with %q in GroupAllowFromExtra (calls=%d)",
			humanMatrix, len(calls))
	})

	// Flip superAdmin off; Manager allowFromExtra should no longer include it.
	baseline := len(mockMgrDeploy.Calls.DeployManagerConfig)
	updateHumanSpec(t, human, func(h *v1beta1.Human) {
		h.Spec.SuperAdmin = false
	})

	assertEventually(t, func() error {
		calls := mockMgrDeploy.Calls.DeployManagerConfig
		for i := len(calls) - 1; i >= baseline; i-- {
			if !containsString(calls[i].GroupAllowFromExtra, humanMatrix) {
				return nil
			}
		}
		return fmt.Errorf("no post-flip DeployManagerConfig call without %q (baseline=%d calls=%d)",
			humanMatrix, baseline, len(calls))
	})
}

// ---------------------------------------------------------------------------
// Manager test helpers
// ---------------------------------------------------------------------------

// managerContainerName mirrors the controller's naming logic for tests.
func managerContainerName(name string) string {
	if name == "default" {
		return "hiclaw-manager"
	}
	return "hiclaw-manager-" + name
}

func waitForManagerRunning(t *testing.T, mgr *v1beta1.Manager) {
	t.Helper()
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("phase=%q message=%q gen=%d obsGen=%d, want Running",
				m.Status.Phase, m.Status.Message, m.Generation, m.Status.ObservedGeneration)
		}
		return nil
	})
}

func updateManagerSpecField(t *testing.T, mgr *v1beta1.Manager, mutate func(m *v1beta1.Manager)) {
	t.Helper()
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		mutate(&m)
		return k8sClient.Update(ctx, &m)
	})
}

func triggerManagerReconcile(t *testing.T, mgr *v1beta1.Manager) {
	t.Helper()
	assertEventually(t, func() error {
		var m v1beta1.Manager
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(mgr), &m); err != nil {
			return err
		}
		if m.Annotations == nil {
			m.Annotations = map[string]string{}
		}
		m.Annotations["hiclaw.io/reconcile-trigger"] = fmt.Sprintf("%d", time.Now().UnixNano())
		return k8sClient.Update(ctx, &m)
	})
}
