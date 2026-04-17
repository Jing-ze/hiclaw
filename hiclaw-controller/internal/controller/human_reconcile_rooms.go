package controller

import (
	"context"
	"sort"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconcileHumanRooms derives the desired set of Matrix rooms for this
// Human from spec (superAdmin / teamAccess / workerAccess) and observed
// CR status, then diffs against Human.status.Rooms to issue Join / Leave.
//
// Desired room sources:
//
//   - superAdmin: every Team Room, every Leader DM Room, every Worker Room.
//   - teamAccess[entry]:
//     - always: entry.Team's Team Room + all member Worker Rooms.
//     - when role=admin: additionally entry.Team's Leader DM Room.
//   - workerAccess[name]: the named Worker's Room.
//
// Missing referenced resources (unresolved Teams or Workers) are skipped
// silently — Human reconciler re-runs on those resources' status changes
// via the Watches registered in SetupWithManager, so soft-references
// converge on their own. Partial JoinRoom / LeaveRoom failures are
// logged and surfaced via status but do not fail the reconcile; rooms
// that succeed are retained in the updated status.Rooms set.
func (r *HumanReconciler) reconcileHumanRooms(ctx context.Context, s *humanScope) (reconcile.Result, error) {
	h := s.human
	logger := log.FromContext(ctx)

	desiredSet, err := r.computeDesiredRooms(ctx, h)
	if err != nil {
		return reconcile.Result{}, err
	}

	currentSet := make(map[string]bool, len(h.Status.Rooms))
	for _, rm := range h.Status.Rooms {
		currentSet[rm] = true
	}

	// Join rooms that are desired but not currently joined.
	for rm := range desiredSet {
		if currentSet[rm] {
			continue
		}
		if err := r.Matrix.JoinRoom(ctx, rm, s.matrixAccessToken); err != nil {
			logger.Error(err, "failed to join room (non-fatal)", "room", rm)
			continue
		}
		currentSet[rm] = true
	}

	// Leave rooms that are currently joined but no longer desired.
	for rm := range currentSet {
		if desiredSet[rm] {
			continue
		}
		if err := r.Matrix.LeaveRoom(ctx, rm, s.matrixAccessToken); err != nil {
			logger.Error(err, "failed to leave room (non-fatal)", "room", rm)
			// Keep tracking in status.Rooms so subsequent reconciles retry.
			continue
		}
		delete(currentSet, rm)
	}

	updated := make([]string, 0, len(currentSet))
	for rm := range currentSet {
		updated = append(updated, rm)
	}
	sort.Strings(updated)
	h.Status.Rooms = updated
	s.desiredRooms = updated

	logger.V(1).Info("rooms reconciled",
		"human", h.Name,
		"desired", len(desiredSet),
		"current", len(updated))
	return reconcile.Result{}, nil
}

// computeDesiredRooms resolves the Human's access spec into a set of
// Matrix room IDs by reading Team and Worker observations.
func (r *HumanReconciler) computeDesiredRooms(ctx context.Context, h *v1beta1.Human) (map[string]bool, error) {
	out := make(map[string]bool)

	// Direct worker-access rooms (standalone and team-member workers alike).
	for _, workerName := range h.Spec.WorkerAccess {
		var w v1beta1.Worker
		if err := r.Get(ctx, client.ObjectKey{Name: workerName, Namespace: h.Namespace}, &w); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return nil, err
			}
			continue
		}
		if w.Status.RoomID != "" {
			out[w.Status.RoomID] = true
		}
	}

	if h.Spec.SuperAdmin {
		if err := r.addAllRoomsToSet(ctx, h.Namespace, out, superAdminSelector{}); err != nil {
			return nil, err
		}
		return out, nil
	}

	for _, entry := range h.Spec.TeamAccess {
		if err := r.addTeamAccessRoomsToSet(ctx, h.Namespace, entry, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// superAdminSelector is a sentinel type marking full-access traversal.
type superAdminSelector struct{}

// addAllRoomsToSet adds all Team Rooms / Leader DM Rooms / Worker Rooms
// in the namespace to the set. Used only for superAdmin Humans.
func (r *HumanReconciler) addAllRoomsToSet(ctx context.Context, ns string, out map[string]bool, _ superAdminSelector) error {
	var tList v1beta1.TeamList
	if err := r.List(ctx, &tList, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range tList.Items {
		t := &tList.Items[i]
		if t.Status.TeamRoomID != "" {
			out[t.Status.TeamRoomID] = true
		}
		if t.Status.LeaderDMRoomID != "" {
			out[t.Status.LeaderDMRoomID] = true
		}
	}
	var wList v1beta1.WorkerList
	if err := r.List(ctx, &wList, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range wList.Items {
		w := &wList.Items[i]
		if w.Status.RoomID != "" {
			out[w.Status.RoomID] = true
		}
	}
	return nil
}

// addTeamAccessRoomsToSet resolves a single teamAccess entry by looking
// up the referenced Team and its observed members, then adds the
// appropriate rooms to the set based on the entry's role.
func (r *HumanReconciler) addTeamAccessRoomsToSet(ctx context.Context, ns string, entry v1beta1.TeamAccessEntry, out map[string]bool) error {
	var t v1beta1.Team
	if err := r.Get(ctx, client.ObjectKey{Name: entry.Team, Namespace: ns}, &t); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return nil
	}
	if t.Status.TeamRoomID != "" {
		out[t.Status.TeamRoomID] = true
	}
	if entry.Role == v1beta1.TeamAccessRoleAdmin && t.Status.LeaderDMRoomID != "" {
		out[t.Status.LeaderDMRoomID] = true
	}
	// Add observed team_worker rooms so the Human can DM individual members.
	for _, m := range t.Status.Members {
		var w v1beta1.Worker
		if err := r.Get(ctx, client.ObjectKey{Name: m.Name, Namespace: ns}, &w); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return err
			}
			continue
		}
		if w.Status.RoomID != "" {
			out[w.Status.RoomID] = true
		}
	}
	// Include the leader's own worker room when admin access.
	if entry.Role == v1beta1.TeamAccessRoleAdmin && t.Status.Leader != nil {
		var w v1beta1.Worker
		if err := r.Get(ctx, client.ObjectKey{Name: t.Status.Leader.Name, Namespace: ns}, &w); err == nil {
			if w.Status.RoomID != "" {
				out[w.Status.RoomID] = true
			}
		}
	}
	return nil
}
