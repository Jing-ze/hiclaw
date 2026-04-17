//go:build integration

package controller_test

import (
	"context"
	"fmt"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/test/testutil/fixtures"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// Test T1: Empty team (no workers, no admins) -> Pending + NoLeader
// ---------------------------------------------------------------------------

func TestTeam_EmptyTeam_PhasePending(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-empty")
	team := fixtures.NewTestTeam(teamName)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, team) })

	// Wait for finalizer as a proxy for "reconciled at least once".
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		for _, f := range got.Finalizers {
			if f == "hiclaw.io/cleanup" {
				return nil
			}
		}
		return fmt.Errorf("finalizer not yet added")
	})

	waitForTeamPhase(t, team, "Pending")

	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got.Status.Leader != nil {
		t.Errorf("Leader=%+v, want nil", got.Status.Leader)
	}
	if len(got.Status.Members) != 0 {
		t.Errorf("Members=%d, want 0", len(got.Status.Members))
	}
	if !hasCondition(got.Status.Conditions, v1beta1.ConditionLeaderResolved, metav1.ConditionFalse, v1beta1.ConditionNoLeader) {
		t.Errorf("expected LeaderResolved=False/NoLeader, got conditions: %+v", got.Status.Conditions)
	}
	// Rooms phase short-circuits when there is no ready leader, so
	// EnsureTeamRooms must NOT be called for a leaderless team.
	ensureRooms, _, _, _ := mockProv.TeamCallCounts()
	if ensureRooms != 0 {
		t.Errorf("EnsureTeamRooms called %d times for leaderless team, want 0", ensureRooms)
	}
	if got.Status.TeamRoomID != "" {
		t.Errorf("TeamRoomID=%q, want empty until leader is ready", got.Status.TeamRoomID)
	}
	if !hasCondition(got.Status.Conditions, v1beta1.ConditionTeamRoomReady, metav1.ConditionFalse, "LeaderNotReady") {
		t.Errorf("expected TeamRoomReady=False/LeaderNotReady, got: %+v", got.Status.Conditions)
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("ObservedGeneration=%d, want %d", got.Status.ObservedGeneration, got.Generation)
	}
}

// ---------------------------------------------------------------------------
// Test T2: Leader-first apply order (Worker created before Team) -> eventually Active.
// ---------------------------------------------------------------------------

func TestTeam_LeaderFirstOrder_EventuallyActive(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-leader1st")
	leaderName := teamName + "-lead"

	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	if err := k8sClient.Create(ctx, leader); err != nil {
		t.Fatalf("create leader worker: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, leader) })
	waitForRunning(t, leader)

	team := fixtures.NewTestTeam(teamName)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, team) })

	waitForTeamPhase(t, team, "Active")
	waitForTeamLeaderReady(t, team)

	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got.Status.Leader == nil || got.Status.Leader.Name != leaderName {
		t.Errorf("Leader=%+v, want Name=%s", got.Status.Leader, leaderName)
	}
	if !hasCondition(got.Status.Conditions, v1beta1.ConditionLeaderResolved, metav1.ConditionTrue, "LeaderFound") {
		t.Errorf("expected LeaderResolved=True/LeaderFound, got: %+v", got.Status.Conditions)
	}
}

// ---------------------------------------------------------------------------
// Test T3: Team-first order (Team created before Worker). Watches(Worker)
// must trigger Team reconcile once the leader appears.
// ---------------------------------------------------------------------------

func TestTeam_TeamFirstOrder_EventuallyActive(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-team1st")
	leaderName := teamName + "-lead"

	team := fixtures.NewTestTeam(teamName)
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, team) })
	waitForTeamPhase(t, team, "Pending")

	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	if err := k8sClient.Create(ctx, leader); err != nil {
		t.Fatalf("create leader worker: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, leader) })
	waitForRunning(t, leader)

	waitForTeamPhase(t, team, "Active")
	waitForTeamLeaderReady(t, team)
}

// ---------------------------------------------------------------------------
// Test T4: Adding a team_worker updates Status.Members and the room desired set.
// ---------------------------------------------------------------------------

func TestTeam_AddMember_UpdatesStatus(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-addmem")
	leaderName := teamName + "-lead"
	memberName := teamName + "-dev"

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	if err := k8sClient.Create(ctx, team); err != nil {
		t.Fatalf("create team: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, team) })
	if err := k8sClient.Create(ctx, leader); err != nil {
		t.Fatalf("create leader: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, leader) })
	waitForRunning(t, leader)
	waitForTeamPhase(t, team, "Active")

	// Record baseline membership call count to assert a NEW membership
	// call happens after the member is added.
	_, membershipBefore, _, _ := mockProv.TeamCallCounts()

	member := fixtures.NewTestWorker(memberName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamWorker),
		fixtures.WithTeamRef(teamName))
	if err := k8sClient.Create(ctx, member); err != nil {
		t.Fatalf("create member: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, member) })
	waitForRunning(t, member)

	waitForTeamMembers(t, team, 1)

	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got.Status.Members[0].Name != memberName {
		t.Errorf("Members[0].Name=%q, want %q", got.Status.Members[0].Name, memberName)
	}
	if got.Status.TotalMembers != 1 {
		t.Errorf("TotalMembers=%d, want 1", got.Status.TotalMembers)
	}
	if got.Status.ReadyMembers != 1 {
		t.Errorf("ReadyMembers=%d, want 1", got.Status.ReadyMembers)
	}

	// A fresh membership reconcile should have fired after member join.
	_, membershipAfter, _, _ := mockProv.TeamCallCounts()
	if membershipAfter <= membershipBefore {
		t.Errorf("ReconcileTeamRoomMembership count=%d, want >%d", membershipAfter, membershipBefore)
	}

	// Some membership call after the member joined should include it.
	memberMatrix := "@" + memberName + ":localhost"
	waitForMembershipCallMatching(t, 0, func(req service.TeamRoomMembershipRequest) bool {
		return containsString(req.DesiredTeamMembers, memberMatrix)
	})
}

// ---------------------------------------------------------------------------
// Test T5: Removing a team_worker shrinks the desired set.
// ---------------------------------------------------------------------------

func TestTeam_RemoveMember_UpdatesStatus(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-rmmem")
	leaderName := teamName + "-lead"
	memberName := teamName + "-dev"

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	member := fixtures.NewTestWorker(memberName,
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

	memberMatrix := "@" + memberName + ":localhost"

	// Record baseline AFTER member is observed in status, so post-delete
	// assertions don't accept pre-member stale calls.
	baseline := membershipCallCount()

	if err := k8sClient.Delete(ctx, member); err != nil {
		t.Fatalf("delete member: %v", err)
	}
	waitForTeamMembers(t, team, 0)

	// A post-delete membership call must NOT include the deleted Matrix ID.
	waitForMembershipCallMatching(t, baseline, func(req service.TeamRoomMembershipRequest) bool {
		return !containsString(req.DesiredTeamMembers, memberMatrix)
	})
}

// ---------------------------------------------------------------------------
// Test T6: teamRef migration alpha→beta. Worker keeps identity; both teams' status update.
// ---------------------------------------------------------------------------

func TestTeam_TeamRefMigration_WorkerSwitchesTeams(t *testing.T) {
	resetAllMocks()

	alpha := fixtures.UniqueName("t-alpha")
	beta := fixtures.UniqueName("t-beta")
	alphaLead := alpha + "-lead"
	betaLead := beta + "-lead"
	workerName := "dev-" + fixtures.UniqueName("shared")

	teamAlpha := fixtures.NewTestTeam(alpha)
	teamBeta := fixtures.NewTestTeam(beta)
	leaderAlpha := fixtures.NewTestWorker(alphaLead,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(alpha))
	leaderBeta := fixtures.NewTestWorker(betaLead,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(beta))
	dev := fixtures.NewTestWorker(workerName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamWorker),
		fixtures.WithTeamRef(alpha))

	for _, obj := range []client.Object{teamAlpha, teamBeta, leaderAlpha, leaderBeta, dev} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T/%s: %v", obj, obj.GetName(), err)
		}
	}
	t.Cleanup(func() {
		for _, obj := range []client.Object{dev, leaderAlpha, leaderBeta, teamAlpha, teamBeta} {
			_ = k8sClient.Delete(ctx, obj)
		}
	})

	waitForRunning(t, leaderAlpha)
	waitForRunning(t, leaderBeta)
	waitForRunning(t, dev)
	waitForTeamMembers(t, teamAlpha, 1)
	waitForTeamMembers(t, teamBeta, 0)

	// Record Provision count before migration — must not increase during migration.
	provBefore, _, _, _ := mockProv.CallCounts()

	// Migrate dev from alpha to beta.
	updateSpecField(t, dev, func(w *v1beta1.Worker) {
		w.Spec.TeamRef = beta
	})

	waitForTeamMembers(t, teamAlpha, 0)
	waitForTeamMembers(t, teamBeta, 1)

	// Worker.Status.TeamRef should now be beta.
	var gotDev v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(dev), &gotDev); err != nil {
		t.Fatalf("get dev: %v", err)
	}
	if gotDev.Status.TeamRef != beta {
		t.Errorf("dev.Status.TeamRef=%q, want %q", gotDev.Status.TeamRef, beta)
	}
	if gotDev.Labels[v1beta1.LabelTeam] != beta {
		t.Errorf("dev.Labels[team]=%q, want %q", gotDev.Labels[v1beta1.LabelTeam], beta)
	}

	// Identity must be preserved — no re-provisioning.
	provAfter, _, _, _ := mockProv.CallCounts()
	if provAfter != provBefore {
		t.Errorf("ProvisionWorker called %d new times during migration, want 0", provAfter-provBefore)
	}
}

// ---------------------------------------------------------------------------
// Test T7: Multi-leader detection. Two team_leader workers for same team
// -> MultipleLeaders condition, Degraded phase.
// ---------------------------------------------------------------------------

func TestTeam_MultipleLeaders_Detected(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-multi")
	leaderA := teamName + "-lead-a"
	leaderB := teamName + "-lead-b"

	team := fixtures.NewTestTeam(teamName)
	lA := fixtures.NewTestWorker(leaderA,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	lB := fixtures.NewTestWorker(leaderB,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))

	for _, obj := range []client.Object{team, lA, lB} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, lA)
		_ = k8sClient.Delete(ctx, lB)
	})

	waitForRunning(t, lA)
	waitForRunning(t, lB)

	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Degraded" {
			return fmt.Errorf("phase=%q, want Degraded", got.Status.Phase)
		}
		if !hasCondition(got.Status.Conditions, v1beta1.ConditionLeaderResolved, metav1.ConditionFalse, v1beta1.ConditionMultipleLeaders) {
			return fmt.Errorf("expected MultipleLeaders condition, got %+v", got.Status.Conditions)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Test T8: Deleting a Team does NOT delete its Workers. Workers survive
// and surface TeamRefResolved=False/TeamNotFound.
// ---------------------------------------------------------------------------

func TestTeam_DeleteTeam_DoesNotDeleteWorkers(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-deltm")
	leaderName := teamName + "-lead"
	memberName := teamName + "-dev"

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	member := fixtures.NewTestWorker(memberName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamWorker),
		fixtures.WithTeamRef(teamName))
	for _, obj := range []client.Object{team, leader, member} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, leader)
		_ = k8sClient.Delete(ctx, member)
	})
	waitForRunning(t, leader)
	waitForRunning(t, member)
	waitForTeamPhase(t, team, "Active")

	// Baseline: DeprovisionWorker must stay at 0 throughout team deletion.
	_, deprovBefore, _, _ := mockProv.CallCounts()
	_, _, _, cleanupBefore := mockProv.TeamCallCounts()

	if err := k8sClient.Delete(ctx, team); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	assertEventually(t, func() error {
		var got v1beta1.Team
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got)
		if err == nil {
			return fmt.Errorf("team still exists")
		}
		return client.IgnoreNotFound(err)
	})

	// Workers must still exist.
	var gotLead v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &gotLead); err != nil {
		t.Fatalf("leader should still exist: %v", err)
	}
	var gotMem v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(member), &gotMem); err != nil {
		t.Fatalf("member should still exist: %v", err)
	}

	// CleanupTeamInfra must have fired exactly once.
	_, _, _, cleanupAfter := mockProv.TeamCallCounts()
	if cleanupAfter-cleanupBefore != 1 {
		t.Errorf("CleanupTeamInfra delta=%d, want 1", cleanupAfter-cleanupBefore)
	}

	// No Worker deprovisioning should have occurred.
	_, deprovAfter, _, _ := mockProv.CallCounts()
	if deprovAfter != deprovBefore {
		t.Errorf("DeprovisionWorker count delta=%d, want 0 (Team deletion must not cascade)", deprovAfter-deprovBefore)
	}

	// Workers should eventually observe TeamRefResolved=False/TeamNotFound.
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &w); err != nil {
			return err
		}
		if !hasCondition(w.Status.Conditions, v1beta1.ConditionTeamRefResolved, metav1.ConditionFalse, "TeamNotFound") {
			return fmt.Errorf("leader conditions=%+v, want TeamRefResolved=False/TeamNotFound", w.Status.Conditions)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Test T9: Recreating a Team with the same name reconverges existing Workers.
// ---------------------------------------------------------------------------

func TestTeam_RecreateTeam_WorkersReconverge(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-recreate")
	leaderName := teamName + "-lead"

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

	// Delete + wait for team to be gone.
	if err := k8sClient.Delete(ctx, team); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	assertEventually(t, func() error {
		var got v1beta1.Team
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got)
		if err == nil {
			return fmt.Errorf("team still exists")
		}
		return client.IgnoreNotFound(err)
	})

	// Leader should surface TeamRefResolved=False.
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &w); err != nil {
			return err
		}
		if !hasCondition(w.Status.Conditions, v1beta1.ConditionTeamRefResolved, metav1.ConditionFalse, "TeamNotFound") {
			return fmt.Errorf("leader not yet degraded: %+v", w.Status.Conditions)
		}
		return nil
	})

	// Recreate the team.
	team2 := fixtures.NewTestTeam(teamName)
	if err := k8sClient.Create(ctx, team2); err != nil {
		t.Fatalf("recreate team: %v", err)
	}

	// Leader should re-resolve and team should become Active again.
	waitForTeamPhase(t, team2, "Active")
	assertEventually(t, func() error {
		var w v1beta1.Worker
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(leader), &w); err != nil {
			return err
		}
		if !hasCondition(w.Status.Conditions, v1beta1.ConditionTeamRefResolved, metav1.ConditionTrue, "TeamFound") {
			return fmt.Errorf("leader did not rediscover team: %+v", w.Status.Conditions)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Test T10: Admin Human appears -> Team.Status.Admins populated.
// ---------------------------------------------------------------------------

func TestTeam_AdminHumanAppears_ProjectedIntoStatus(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-admin")
	leaderName := teamName + "-lead"
	humanName := fixtures.UniqueName("h-admin")

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

	// Now create the admin Human.
	human := fixtures.NewTestHuman(humanName,
		fixtures.WithTeamAccess(teamName, v1beta1.TeamAccessRoleAdmin))
	if err := k8sClient.Create(ctx, human); err != nil {
		t.Fatalf("create human: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, human) })
	waitForHumanPhase(t, human, "Active")

	waitForTeamAdmins(t, team, 1)

	var got v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got.Status.Admins[0].HumanName != humanName {
		t.Errorf("Admins[0].HumanName=%q, want %q", got.Status.Admins[0].HumanName, humanName)
	}
	adminMatrix := "@" + humanName + ":localhost"
	if got.Status.Admins[0].MatrixUserID != adminMatrix {
		t.Errorf("Admins[0].MatrixUserID=%q, want %q", got.Status.Admins[0].MatrixUserID, adminMatrix)
	}

	// A post-admin membership call must list the admin in DesiredLeaderDMUsers.
	waitForMembershipCallMatching(t, 0, func(req service.TeamRoomMembershipRequest) bool {
		return containsString(req.DesiredLeaderDMUsers, adminMatrix)
	})
}

// ---------------------------------------------------------------------------
// Test T11: Removing the admin teamAccess entry shrinks Team.status.Admins.
// ---------------------------------------------------------------------------

func TestTeam_AdminHumanRemoved_AdminsShrinks(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-admrm")
	leaderName := teamName + "-lead"
	humanName := fixtures.UniqueName("h-admrm")

	team := fixtures.NewTestTeam(teamName)
	leader := fixtures.NewTestWorker(leaderName,
		fixtures.WithRole(v1beta1.WorkerRoleTeamLeader),
		fixtures.WithTeamRef(teamName))
	human := fixtures.NewTestHuman(humanName,
		fixtures.WithTeamAccess(teamName, v1beta1.TeamAccessRoleAdmin))
	for _, obj := range []client.Object{team, leader, human} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, team)
		_ = k8sClient.Delete(ctx, leader)
		_ = k8sClient.Delete(ctx, human)
	})
	waitForRunning(t, leader)
	waitForHumanPhase(t, human, "Active")
	waitForTeamAdmins(t, team, 1)

	// Strip the teamAccess entry.
	updateHumanSpec(t, human, func(h *v1beta1.Human) {
		h.Spec.TeamAccess = nil
	})

	waitForTeamAdmins(t, team, 0)
}

// ---------------------------------------------------------------------------
// Test T12: Leader provisioning fails -> Leader.Ready=false -> Phase stays
// Pending even though leader is observable.
// ---------------------------------------------------------------------------

func TestTeam_LeaderNotReady_PhaseStaysPending(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-notready")
	leaderName := teamName + "-lead"

	// Force ProvisionWorker to fail only for this leader.
	mockProv.ProvisionWorkerFn = func(_ context.Context, req service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error) {
		if req.Name == leaderName {
			return nil, fmt.Errorf("simulated leader provision failure")
		}
		return &service.WorkerProvisionResult{
			MatrixUserID:   "@" + req.Name + ":localhost",
			MatrixToken:    "mock-token-" + req.Name,
			RoomID:         "!room-" + req.Name + ":localhost",
			GatewayKey:     "mock-gw-key-" + req.Name,
			MinIOPassword:  "mock-minio-pw",
			MatrixPassword: "mock-matrix-pw",
		}, nil
	}

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

	// Leader never becomes ready.
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.Phase != "Pending" {
			return fmt.Errorf("phase=%q, want Pending", got.Status.Phase)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Test T13: EnsureTeamRooms is idempotent — subsequent reconciles pass
// existing room IDs rather than creating new ones.
// ---------------------------------------------------------------------------

func TestTeam_RoomsIdempotent_NoRecreate(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-idemp")
	leaderName := teamName + "-lead"

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

	var firstGot v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &firstGot); err != nil {
		t.Fatalf("get team: %v", err)
	}
	initialTeamRoom := firstGot.Status.TeamRoomID
	initialLeaderDMRoom := firstGot.Status.LeaderDMRoomID
	if initialTeamRoom == "" || initialLeaderDMRoom == "" {
		t.Fatalf("missing initial room IDs: %+v", firstGot.Status)
	}

	// Wait until at least one EnsureTeamRooms call has passed the
	// initial room IDs — proving the reconciler observed Status.rooms
	// from an earlier iteration and short-circuits room creation.
	waitForRoomsCallMatching(t, func(req service.TeamRoomsRequest) bool {
		return req.ExistingTeamRoomID == initialTeamRoom &&
			req.ExistingLeaderDMRoomID == initialLeaderDMRoom
	})

	// Room IDs in status must not have changed.
	var latest v1beta1.Team
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &latest); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if latest.Status.TeamRoomID != initialTeamRoom {
		t.Errorf("TeamRoomID changed: %q -> %q", initialTeamRoom, latest.Status.TeamRoomID)
	}
	if latest.Status.LeaderDMRoomID != initialLeaderDMRoom {
		t.Errorf("LeaderDMRoomID changed: %q -> %q", initialLeaderDMRoom, latest.Status.LeaderDMRoomID)
	}
}

// ---------------------------------------------------------------------------
// Test T14: ObservedGeneration is only advanced on successful reconcile.
// Needs a leader so that reconcileRooms actually invokes EnsureTeamRooms.
// ---------------------------------------------------------------------------

func TestTeam_ObservedGeneration_OnlyOnSuccess(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-obsgen")
	leaderName := teamName + "-lead"

	// First EnsureTeamRooms fails, subsequent succeed.
	callCount := 0
	mockProv.EnsureTeamRoomsFn = func(_ context.Context, req service.TeamRoomsRequest) (*service.TeamRoomsResult, error) {
		callCount++
		if callCount <= 1 {
			return nil, fmt.Errorf("simulated room failure")
		}
		return &service.TeamRoomsResult{
			TeamRoomID:     "!team-" + req.TeamName + ":localhost",
			LeaderDMRoomID: "!leader-dm-" + req.TeamName + ":localhost",
		}, nil
	}

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

	// Eventually reconcile succeeds and ObservedGeneration catches up.
	assertEventually(t, func() error {
		var got v1beta1.Team
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(team), &got); err != nil {
			return err
		}
		if got.Status.ObservedGeneration != got.Generation {
			return fmt.Errorf("ObservedGeneration=%d gen=%d msg=%q",
				got.Status.ObservedGeneration, got.Generation, got.Status.Message)
		}
		if got.Status.TeamRoomID == "" {
			return fmt.Errorf("rooms not set yet")
		}
		return nil
	})
	if callCount < 2 {
		t.Errorf("EnsureTeamRoomsFn callCount=%d, want >=2 (first fails, then succeeds)", callCount)
	}
}

// ---------------------------------------------------------------------------
// Test T15: Storage ensure failure is non-critical — Team still becomes Active.
// ---------------------------------------------------------------------------

func TestTeam_StorageEnsuredBestEffort(t *testing.T) {
	resetAllMocks()

	teamName := fixtures.UniqueName("t-storage")
	leaderName := teamName + "-lead"

	mockProv.EnsureTeamStorageFn = func(_ context.Context, _ string) error {
		return fmt.Errorf("simulated storage failure")
	}

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

	// Team still reaches Active despite storage errors.
	waitForTeamPhase(t, team, "Active")
}

// ---------------------------------------------------------------------------
// Helpers local to team_test.go
// ---------------------------------------------------------------------------

// hasCondition reports whether the slice contains the expected (type,status,reason).
func hasCondition(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == status && c.Reason == reason {
			return true
		}
	}
	return false
}

// containsString is a small helper for slice membership checks.
func containsString(slice []string, target string) bool {
	for _, v := range slice {
		if v == target {
			return true
		}
	}
	return false
}

// waitForMembershipCallMatching polls the shared mockProv until at least
// one recorded ReconcileTeamRoomMembership request satisfies predicate.
// Only calls at or after baseline index are considered, so tests that
// mutate topology mid-test can exclude pre-mutation history.
func waitForMembershipCallMatching(t *testing.T, baseline int, predicate func(service.TeamRoomMembershipRequest) bool) service.TeamRoomMembershipRequest {
	t.Helper()
	var matched service.TeamRoomMembershipRequest
	assertEventually(t, func() error {
		calls := mockProv.Calls.ReconcileTeamRoomMembership
		for i := len(calls) - 1; i >= baseline; i-- {
			if predicate(calls[i]) {
				matched = calls[i]
				return nil
			}
		}
		return fmt.Errorf("no matching ReconcileTeamRoomMembership in %d calls (baseline=%d)", len(calls), baseline)
	})
	return matched
}

// membershipCallCount returns the current number of recorded membership calls.
func membershipCallCount() int {
	return len(mockProv.Calls.ReconcileTeamRoomMembership)
}

// waitForRoomsCallMatching does the same for EnsureTeamRooms calls.
func waitForRoomsCallMatching(t *testing.T, predicate func(service.TeamRoomsRequest) bool) service.TeamRoomsRequest {
	t.Helper()
	var matched service.TeamRoomsRequest
	assertEventually(t, func() error {
		calls := mockProv.Calls.EnsureTeamRooms
		for i := len(calls) - 1; i >= 0; i-- {
			if predicate(calls[i]) {
				matched = calls[i]
				return nil
			}
		}
		return fmt.Errorf("no matching EnsureTeamRooms in %d calls", len(calls))
	})
	return matched
}
