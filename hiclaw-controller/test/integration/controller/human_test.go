//go:build integration

package controller_test

import (
	"fmt"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// Test H1: Plain Human creation -> Active + Matrix account provisioned.
// ---------------------------------------------------------------------------

func TestHuman_Create_HappyPath(t *testing.T) {
	resetAllMocks()

	humanName := fixtures.UniqueName("h-create")
	human := fixtures.NewTestHuman(humanName)
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })

	waitForHumanPhase(t, human, "Active")

	var got v1beta1.Human
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got); err != nil {
		t.Fatalf("get human: %v", err)
	}

	wantMatrixID := "@" + humanName + ":localhost"
	if got.Status.MatrixUserID != wantMatrixID {
		t.Errorf("MatrixUserID=%q, want %q", got.Status.MatrixUserID, wantMatrixID)
	}
	if got.Status.InitialPassword == "" {
		t.Error("InitialPassword should be set after provisioning")
	}
	if len(got.Status.Rooms) != 0 {
		t.Errorf("Rooms=%v, want empty (no access declarations)", got.Status.Rooms)
	}
	if !containsString(mockMatrix.Calls.EnsureUser, humanName) {
		t.Errorf("mockMatrix.Calls.EnsureUser=%v, missing %q", mockMatrix.Calls.EnsureUser, humanName)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("ObservedGeneration=%d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}

// ---------------------------------------------------------------------------
// Test H2: Subsequent reconciles reuse status.initialPassword rather than
// regenerating — prevents drift between the displayed password and the
// actual Matrix credential on requeue.
// ---------------------------------------------------------------------------

func TestHuman_InitialPasswordReuse(t *testing.T) {
	resetAllMocks()

	humanName := fixtures.UniqueName("h-pwreuse")
	human := fixtures.NewTestHuman(humanName)
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	var initial v1beta1.Human
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &initial); err != nil {
		t.Fatalf("get human: %v", err)
	}
	initialPassword := initial.Status.InitialPassword
	if initialPassword == "" {
		t.Fatal("initial password not set")
	}

	// Bump annotation to force a reconcile.
	updateHumanSpec(t, human, func(h *v1beta1.Human) {
		if h.Annotations == nil {
			h.Annotations = map[string]string{}
		}
		h.Annotations["hiclaw.io/reconcile-trigger"] = "bump"
	})

	// Wait until at least two EnsureUser calls have been recorded.
	assertEventually(t, func() error {
		count := 0
		for _, name := range mockMatrix.Calls.EnsureUser {
			if name == humanName {
				count++
			}
		}
		if count < 2 {
			return fmt.Errorf("EnsureUser calls for %s=%d, want >=2", humanName, count)
		}
		return nil
	})

	// Status.InitialPassword must not have changed.
	var later v1beta1.Human
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &later); err != nil {
		t.Fatalf("get human: %v", err)
	}
	if later.Status.InitialPassword != initialPassword {
		t.Errorf("InitialPassword rotated unexpectedly: %q -> %q", initialPassword, later.Status.InitialPassword)
	}
}

// ---------------------------------------------------------------------------
// Test H3: superAdmin=true Human joins every Team room + Worker room.
// ---------------------------------------------------------------------------

func TestHuman_SuperAdmin_JoinsAllTeamRooms(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("h-sa-team")
	leaderName := teamName + "-lead"
	humanName := fixtures.UniqueName("h-sa")

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	for _, obj := range []client.Object{team, leader} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
	})
	waitForRunning(t, leader)
	waitForTeamPhase(t, team, "Active")

	human := fixtures.NewTestHuman(humanName, fixtures.WithSuperAdmin())
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	teamRoom := "!team-" + teamName + ":localhost"
	leaderDMRoom := "!leader-dm-" + teamName + ":localhost"
	leaderWorkerRoom := "!room-" + leaderName + ":localhost"

	waitForHumanInRooms(t, human, teamRoom, leaderDMRoom, leaderWorkerRoom)
}

// ---------------------------------------------------------------------------
// Test H4: teamAccess admin Human joins team room, leader DM room, and
// all member worker rooms + the leader worker room.
// ---------------------------------------------------------------------------

func TestHuman_TeamAccessAdmin_JoinsTeamAndDM(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("h-adm-team")
	leaderName := teamName + "-lead"
	humanName := fixtures.UniqueName("h-adm")

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	for _, obj := range []client.Object{team, leader} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
	})
	waitForRunning(t, leader)
	waitForTeamPhase(t, team, "Active")

	human := fixtures.NewTestHuman(humanName,
		fixtures.WithTeamAccess(teamName, v1beta1.TeamAccessRoleAdmin))
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	teamRoom := "!team-" + teamName + ":localhost"
	leaderDMRoom := "!leader-dm-" + teamName + ":localhost"
	leaderWorkerRoom := "!room-" + leaderName + ":localhost"

	waitForHumanInRooms(t, human, teamRoom, leaderDMRoom, leaderWorkerRoom)
}

// ---------------------------------------------------------------------------
// Test H5: teamAccess member Human joins team room + member worker rooms
// but NOT the leader DM room.
// ---------------------------------------------------------------------------

func TestHuman_TeamAccessMember_JoinsTeamOnly(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("h-mem-team")
	leaderName := teamName + "-lead"
	workerName := teamName + "-dev"
	humanName := fixtures.UniqueName("h-mem")

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	member := fixtures.NewTestWorker(workerName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamWorker),
		fixtures.WithTeamRef(teamName))
	for _, obj := range []client.Object{team, leader, member} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
		_ = k8sClient.Delete(ctx, member)
	})
	waitForRunning(t, leader)
	waitForRunning(t, member)
	waitForTeamMembers(t, team, 1)
	waitForTeamPhase(t, team, "Active")

	human := fixtures.NewTestHuman(humanName,
		fixtures.WithTeamAccess(teamName, v1beta1.TeamAccessRoleMember))
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	teamRoom := "!team-" + teamName + ":localhost"
	leaderDMRoom := "!leader-dm-" + teamName + ":localhost"
	memberWorkerRoom := "!room-" + workerName + ":localhost"

	waitForHumanInRooms(t, human, teamRoom, memberWorkerRoom)
	// Member role must NOT gain leader DM access.
	waitForHumanNotInRoom(t, human, leaderDMRoom)
}

// ---------------------------------------------------------------------------
// Test H6: workerAccess only covers the named Worker, not other Workers.
// ---------------------------------------------------------------------------

func TestHuman_WorkerAccess_JoinsOnlyNamedWorker(t *testing.T) {
	resetAllMocks()

	workerA := fixtures.UniqueName("h-wa-a")
	workerB := fixtures.UniqueName("h-wa-b")
	humanName := fixtures.UniqueName("h-wa")

	wa := fixtures.NewTestWorker(workerA)
	wb := fixtures.NewTestWorker(workerB)
	for _, obj := range []client.Object{wa, wb} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, wa)
		_ = k8sClient.Delete(ctx, wb)
	})
	waitForRunning(t, wa)
	waitForRunning(t, wb)

	human := fixtures.NewTestHuman(humanName, fixtures.WithWorkerAccess(workerA))
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	roomA := "!room-" + workerA + ":localhost"
	roomB := "!room-" + workerB + ":localhost"

	waitForHumanInRooms(t, human, roomA)
	waitForHumanNotInRoom(t, human, roomB)
}

// ---------------------------------------------------------------------------
// Test H7: Switching teamAccess from alpha to beta issues Leave on alpha's
// rooms and Join on beta's.
// ---------------------------------------------------------------------------

func TestHuman_TeamAccessChange_LeavesAndJoins(t *testing.T) {
	resetAllMocks()

	alpha := fixtures.UniqueName("h-ta-a")
	beta := fixtures.UniqueName("h-ta-b")
	alphaLead := alpha + "-lead"
	betaLead := beta + "-lead"
	humanName := fixtures.UniqueName("h-ta")

	teamA := fixtures.NewTestTeam(alpha)
	teamB := fixtures.NewTestTeam(beta)
	leaderA := fixtures.NewTestWorker(alphaLead,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(alpha))
	leaderB := fixtures.NewTestWorker(betaLead,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(beta))
	for _, obj := range []client.Object{teamA, teamB, leaderA, leaderB} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		for _, obj := range []client.Object{leaderA, leaderB, teamA, teamB} {
			_ = k8sClient.Delete(ctx, obj)
		}
	})
	waitForRunning(t, leaderA)
	waitForRunning(t, leaderB)
	waitForTeamPhase(t, teamA, "Active")
	waitForTeamPhase(t, teamB, "Active")

	human := fixtures.NewTestHuman(humanName,
		fixtures.WithTeamAccess(alpha, v1beta1.TeamAccessRoleAdmin))
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	alphaTeamRoom := "!team-" + alpha + ":localhost"
	alphaLeaderDM := "!leader-dm-" + alpha + ":localhost"
	betaTeamRoom := "!team-" + beta + ":localhost"
	betaLeaderDM := "!leader-dm-" + beta + ":localhost"

	waitForHumanInRooms(t, human, alphaTeamRoom, alphaLeaderDM)

	// Switch access from alpha to beta.
	updateHumanSpec(t, human, func(h *v1beta1.Human) {
		h.Spec.TeamAccess = []v1beta1.TeamAccessEntry{
			{Team: beta, Role: v1beta1.TeamAccessRoleAdmin},
		}
	})

	waitForHumanInRooms(t, human, betaTeamRoom, betaLeaderDM)
	waitForHumanNotInRoom(t, human, alphaTeamRoom)
	waitForHumanNotInRoom(t, human, alphaLeaderDM)
}

// ---------------------------------------------------------------------------
// Test H8: Toggling superAdmin flag recomputes rooms — after setting true,
// the Human joins every team's rooms.
// ---------------------------------------------------------------------------

func TestHuman_SuperAdminToggle_RecomputesRooms(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("h-sat-team")
	leaderName := teamName + "-lead"
	humanName := fixtures.UniqueName("h-sat")

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	for _, obj := range []client.Object{team, leader} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
	})
	waitForRunning(t, leader)
	waitForTeamPhase(t, team, "Active")

	// Human starts with no access.
	human := fixtures.NewTestHuman(humanName)
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	teamRoom := "!team-" + teamName + ":localhost"

	// Initially NOT in the team room.
	waitForHumanNotInRoom(t, human, teamRoom)

	// Flip superAdmin on.
	updateHumanSpec(t, human, func(h *v1beta1.Human) {
		h.Spec.SuperAdmin = true
	})

	waitForHumanInRooms(t, human, teamRoom)
}

// ---------------------------------------------------------------------------
// Test H9: Deleting a Human deactivates the Matrix user and clears the
// finalizer, allowing K8s to GC the CR.
// ---------------------------------------------------------------------------

func TestHuman_Delete_DeactivatesAndCleans(t *testing.T) {
	resetAllMocks()

	humanName := fixtures.UniqueName("h-del")
	human := fixtures.NewTestHuman(humanName)
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	waitForHumanPhase(t, human, "Active")

	if err := k8sClient.Delete(ctx, human); err != nil {
		t.Fatalf("delete human: %v", err)
	}

	assertEventually(t, func() error {
		var got v1beta1.Human
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(human), &got)
		if err == nil {
			return fmt.Errorf("human still exists")
		}
		return client.IgnoreNotFound(err)
	})

	if !containsString(mockMatrix.Calls.DeactivateUser, humanName) {
		t.Errorf("DeactivateUser calls=%v, missing %q", mockMatrix.Calls.DeactivateUser, humanName)
	}
}

// ---------------------------------------------------------------------------
// Test H10: Human with workerAccess referencing a not-yet-created Worker
// auto-joins the Worker's room once it appears. Exercises Watches(Worker)
// wiring on HumanReconciler.
// ---------------------------------------------------------------------------

func TestHuman_WorkerRoomAppearsLate_AutoJoin(t *testing.T) {
	resetAllMocks()

	workerName := fixtures.UniqueName("h-late-w")
	humanName := fixtures.UniqueName("h-late-h")

	// Create Human BEFORE Worker — workerAccess references a non-existent
	// Worker at first, which should be tolerated.
	human := fixtures.NewTestHuman(humanName, fixtures.WithWorkerAccess(workerName))
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	// Now create the Worker; Human should gain the room via Watches.
	worker := fixtures.NewTestWorker(workerName)
	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, worker) })
	waitForRunning(t, worker)

	expectedRoom := "!room-" + workerName + ":localhost"
	waitForHumanInRooms(t, human, expectedRoom)
}
