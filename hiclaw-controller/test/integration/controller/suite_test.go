//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/controller"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
	"go.uber.org/zap/zapcore"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	timeout  = 30 * time.Second
	interval = 250 * time.Millisecond
)

var (
	testEnv   *envtest.Environment
	restCfg   *rest.Config // shared with leaderelection_test.go
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc

	// Worker mocks
	mockProv    *mocks.MockProvisioner
	mockDeploy  *mocks.MockDeployer
	mockBackend *mocks.MockWorkerBackend
	mockEnv     *mocks.MockEnvBuilder

	// Manager mocks
	mockMgrProv    *mocks.MockManagerProvisioner
	mockMgrDeploy  *mocks.MockManagerDeployer
	mockMgrBackend *mocks.MockWorkerBackend
	mockMgrEnv     *mocks.MockManagerEnvBuilder

	// Team / Human mocks introduced by the Stage-12 refactor.
	mockMatrix       *mocks.MockMatrixClient
	mockTeamObserver *mocks.MockTeamObserver
	realObserver     *service.Observer
)

func TestMain(m *testing.M) {
	testEnv = testutil.NewTestEnv()
	scheme := testutil.Scheme()

	var err error
	restCfg, err = testEnv.Start()
	if err != nil {
		panic(fmt.Sprintf("failed to start envtest: %v", err))
	}

	ctx, cancel = context.WithCancel(context.Background())
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.Level(zapcore.InfoLevel)))

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics server in tests
		},
	})
	if err != nil {
		panic(fmt.Sprintf("failed to create manager: %v", err))
	}

	// Create a cacheless client so tests always read the latest state.
	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(fmt.Sprintf("failed to create k8s client: %v", err))
	}

	// Wire up Worker mocks
	mockProv = mocks.NewMockProvisioner()
	mockDeploy = mocks.NewMockDeployer()
	mockBackend = mocks.NewMockWorkerBackend()
	mockEnv = mocks.NewMockEnvBuilder()

	workerBackendRegistry := backend.NewRegistry(
		[]backend.WorkerBackend{mockBackend},
		nil,
	)

	workerReconciler := &controller.WorkerReconciler{
		Client:      mgr.GetClient(),
		Provisioner: mockProv,
		Deployer:    mockDeploy,
		Backend:     workerBackendRegistry,
		EnvBuilder:  mockEnv,
		Legacy:      nil,
	}
	if err := workerReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup WorkerReconciler: %v", err))
	}

	// Wire up Manager mocks
	mockMgrProv = mocks.NewMockManagerProvisioner()
	mockMgrDeploy = mocks.NewMockManagerDeployer()
	mockMgrBackend = mocks.NewMockWorkerBackend()
	mockMgrEnv = mocks.NewMockManagerEnvBuilder()

	mgrBackendRegistry := backend.NewRegistry(
		[]backend.WorkerBackend{mockMgrBackend},
		nil,
	)

	managerReconciler := &controller.ManagerReconciler{
		Client:      mgr.GetClient(),
		Provisioner: mockMgrProv,
		Deployer:    mockMgrDeploy,
		Backend:     mgrBackendRegistry,
		EnvBuilder:  mockMgrEnv,
	}
	if err := managerReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup ManagerReconciler: %v", err))
	}

	// Wire up Team / Human reconcilers (Stage 12).
	mockMatrix = mocks.NewMockMatrixClient()
	mockTeamObserver = mocks.NewMockTeamObserver()
	realObserver = service.NewObserver(service.ObserverConfig{Client: mgr.GetClient()})

	teamReconciler := &controller.TeamReconciler{
		Client:      mgr.GetClient(),
		Provisioner: mockProv,
		Observer:    realObserver,
		Legacy:      nil,
	}
	if err := teamReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup TeamReconciler: %v", err))
	}

	humanReconciler := &controller.HumanReconciler{
		Client: mgr.GetClient(),
		Matrix: mockMatrix,
		Legacy: nil,
	}
	if err := humanReconciler.SetupWithManager(mgr); err != nil {
		panic(fmt.Sprintf("failed to setup HumanReconciler: %v", err))
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic(fmt.Sprintf("failed to start manager: %v", err))
		}
	}()

	// Wait for manager cache to sync
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		panic("cache sync failed")
	}

	code := m.Run()

	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop envtest: %v\n", err)
	}

	os.Exit(code)
}

// resetMocks resets all Worker-scope mock call records and Fn overrides.
func resetMocks() {
	mockProv.Reset()
	mockDeploy.Reset()
	mockBackend.Reset()
	mockEnv.Reset()
}

// resetManagerMocks resets all Manager mock call records and Fn overrides.
func resetManagerMocks() {
	mockMgrProv.Reset()
	mockMgrDeploy.Reset()
	mockMgrBackend.Reset()
	mockMgrEnv.Reset()
}

// resetTeamMocks clears mock call records related to Team reconciliation
// (shared MockProvisioner's team-scope calls + matrix membership state).
// It does NOT reset Worker-scope state, since Team tests typically
// exercise Worker reconciler too.
func resetTeamMocks() {
	mockProv.ClearCalls()
	mockMatrix.ClearCalls()
	mockTeamObserver.Reset()
}

// resetHumanMocks clears only Human-reconciler-relevant mock state.
func resetHumanMocks() {
	mockMatrix.Reset()
	mockProv.ClearCalls()
	mockDeploy.ClearCalls()
}

// resetAllMocks fully resets every mock to its initial state. Use at the
// top of tests that span Worker + Team + Human concerns.
func resetAllMocks() {
	resetMocks()
	resetManagerMocks()
	mockMatrix.Reset()
	mockTeamObserver.Reset()
}

// suppress unused import for v1beta1
var _ = v1beta1.GroupName

// -----------------------------------------------------------------------------
// Cross-CR wait helpers (shared by team_test.go / human_test.go / bundle_test.go)
// -----------------------------------------------------------------------------

// waitForTeamPhase polls until team.Status.Phase == phase or timeout.
func waitForTeamPhase(t *testing.T, team *v1beta1.Team, phase string) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if string(got.Status.Phase) != phase {
			return fmt.Errorf("phase=%q message=%q, want %q", got.Status.Phase, got.Status.Message, phase)
		}
		return nil
	})
}

// waitForTeamLeaderReady polls until Team.Status.Leader != nil && Ready.
func waitForTeamLeaderReady(t *testing.T, team *v1beta1.Team) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Leader == nil {
			return fmt.Errorf("leader still nil")
		}
		if !got.Status.Leader.Ready {
			return fmt.Errorf("leader %q not ready yet", got.Status.Leader.Name)
		}
		return nil
	})
}

// waitForTeamMembers polls until Team.Status.Members has exactly count entries.
func waitForTeamMembers(t *testing.T, team *v1beta1.Team, count int) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if len(got.Status.Members) != count {
			names := make([]string, 0, len(got.Status.Members))
			for _, mbr := range got.Status.Members {
				names = append(names, mbr.Name)
			}
			return fmt.Errorf("members=%d (%v), want %d", len(got.Status.Members), names, count)
		}
		return nil
	})
}

// waitForTeamAdmins polls until Team.Status.Admins has exactly count entries.
func waitForTeamAdmins(t *testing.T, team *v1beta1.Team, count int) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if len(got.Status.Admins) != count {
			return fmt.Errorf("admins=%d, want %d", len(got.Status.Admins), count)
		}
		return nil
	})
}

// waitForHumanPhase polls until human.Status.Phase == phase or timeout.
func waitForHumanPhase(t *testing.T, human *v1beta1.Human, phase string) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Human
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got); err != nil {
			return err
		}
		if string(got.Status.Phase) != phase {
			return fmt.Errorf("phase=%q, want %q", got.Status.Phase, phase)
		}
		return nil
	})
}

// waitForHumanInRooms polls until the Human's status.Rooms contains every
// expected roomID (superset match — the Human may be in additional rooms).
func waitForHumanInRooms(t *testing.T, human *v1beta1.Human, roomIDs ...string) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Human
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got); err != nil {
			return err
		}
		set := make(map[string]bool, len(got.Status.Rooms))
		for _, r := range got.Status.Rooms {
			set[r] = true
		}
		for _, want := range roomIDs {
			if !set[want] {
				return fmt.Errorf("room %q not in %v", want, got.Status.Rooms)
			}
		}
		return nil
	})
}

// waitForHumanNotInRoom polls until the Human's status.Rooms does NOT contain roomID.
func waitForHumanNotInRoom(t *testing.T, human *v1beta1.Human, roomID string) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Human
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got); err != nil {
			return err
		}
		for _, r := range got.Status.Rooms {
			if r == roomID {
				return fmt.Errorf("room %q still present in %v", roomID, got.Status.Rooms)
			}
		}
		return nil
	})
}

// updateTeamSpec performs a read-modify-write on a Team, retrying on conflict.
func updateTeamSpec(t *testing.T, team *v1beta1.Team, mutate func(*v1beta1.Team)) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		mutate(&got)
		return k8sClient.Update(ctx, &got)
	})
}

// updateHumanSpec performs a read-modify-write on a Human, retrying on conflict.
func updateHumanSpec(t *testing.T, human *v1beta1.Human, mutate func(*v1beta1.Human)) {
	t.Helper()
	assertEventually(t, func() error {
		var got v1beta1.Human
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got); err != nil {
			return err
		}
		mutate(&got)
		return k8sClient.Update(ctx, &got)
	})
}

// createAndWaitFinalizer creates obj and waits until the hiclaw.io/cleanup
// finalizer is present, which implies the reconciler has observed the
// object at least once.
func createAndWaitFinalizer(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(ctx, obj); err != nil {
		t.Fatalf("create %T/%s: %v", obj, obj.GetName(), err)
	}
	assertEventually(t, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		for _, f := range obj.GetFinalizers() {
			if f == "hiclaw.io/cleanup" {
				return nil
			}
		}
		return fmt.Errorf("finalizer not yet added")
	})
}
