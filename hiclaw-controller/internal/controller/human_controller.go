package controller

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HumanReconciler reconciles Human resources under the new reference-based
// access model: access is declared via spec.superAdmin / spec.teamAccess /
// spec.workerAccess, and Matrix Room membership is computed by observing
// Team.status and Worker.status rather than by any reverse mutation on
// those resources.
type HumanReconciler struct {
	client.Client

	Matrix matrix.Client
	Legacy *service.LegacyCompat // nil in incluster mode
}

// Reconcile implements the level-triggered convergence loop for Humans,
// following the same pattern as Worker / Manager / Team reconcilers:
// patchBase capture, defer-patch status, ObservedGeneration only on
// success, finalizer handled via reconcileHumanDelete.
func (r *HumanReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	logger := log.FromContext(ctx)

	var human v1beta1.Human
	if err := r.Get(ctx, req.NamespacedName, &human); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	patchBase := client.MergeFrom(human.DeepCopy())
	s := &humanScope{human: &human, patchBase: patchBase}

	defer func() {
		if !human.DeletionTimestamp.IsZero() {
			return
		}
		human.Status.Phase = computeHumanPhase(&human, reterr)
		if reterr == nil {
			human.Status.ObservedGeneration = human.Generation
			human.Status.Message = ""
		} else {
			human.Status.Message = reterr.Error()
		}
		if err := r.Status().Patch(ctx, &human, patchBase); err != nil {
			logger.Error(err, "failed to patch human status")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if !human.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&human, finalizerName) {
			return r.reconcileHumanDelete(ctx, s)
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&human, finalizerName) {
		controllerutil.AddFinalizer(&human, finalizerName)
		if err := r.Update(ctx, &human); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileHumanNormal(ctx, s)
}

// reconcileHumanNormal runs the declarative convergence phases:
// infrastructure → rooms → legacy. Infrastructure and rooms are the
// critical path (errors abort); legacy registry update is non-critical.
func (r *HumanReconciler) reconcileHumanNormal(ctx context.Context, s *humanScope) (reconcile.Result, error) {
	if res, err := r.reconcileHumanInfrastructure(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if res, err := r.reconcileHumanRooms(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	r.reconcileHumanLegacy(ctx, s)

	h := s.human
	logger := log.FromContext(ctx)
	if h.Status.ObservedGeneration == 0 {
		logger.Info("human created", "name", h.Name, "matrixUserID", h.Status.MatrixUserID)
	} else if h.Generation != h.Status.ObservedGeneration {
		logger.Info("human updated", "name", h.Name)
	}
	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

// SetupWithManager wires Human reconciliation with cross-CR Watches on
// Team (so status.teamRoomID / status.leaderDMRoomID changes trigger
// Human re-reconcile to join new rooms) and Worker (so status.roomID or
// teamRef changes trigger re-reconcile for Humans whose access scope
// covers that Worker).
func (r *HumanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Human{}).
		Watches(
			&v1beta1.Team{},
			handler.EnqueueRequestsFromMapFunc(r.teamToHumansMapper),
			builder.WithPredicates(teamRoomsChangedPredicates()),
		).
		Watches(
			&v1beta1.Worker{},
			handler.EnqueueRequestsFromMapFunc(r.workerToHumansMapper),
			builder.WithPredicates(workerRoomChangedPredicates()),
		).
		Complete(r)
}

// teamToHumansMapper emits a reconcile request for every Human that has
// a teamAccess entry referencing the changed Team, plus all superAdmin
// Humans (who follow every Team). Listing all Humans on every Team
// change is O(H) per event; acceptable at normal scale.
func (r *HumanReconciler) teamToHumansMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	t, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	var list v1beta1.HumanList
	if err := r.List(ctx, &list, client.InNamespace(t.Namespace)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0)
	for i := range list.Items {
		h := &list.Items[i]
		if h.Spec.SuperAdmin {
			reqs = append(reqs, humanRequest(h))
			continue
		}
		for _, entry := range h.Spec.TeamAccess {
			if entry.Team == t.Name {
				reqs = append(reqs, humanRequest(h))
				break
			}
		}
	}
	return reqs
}

// workerToHumansMapper emits a reconcile request for every Human whose
// workerAccess lists the changed Worker directly, whose teamAccess
// targets the Worker's team, or who is a superAdmin.
func (r *HumanReconciler) workerToHumansMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	w, ok := obj.(*v1beta1.Worker)
	if !ok {
		return nil
	}
	var list v1beta1.HumanList
	if err := r.List(ctx, &list, client.InNamespace(w.Namespace)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0)
	for i := range list.Items {
		h := &list.Items[i]
		if h.Spec.SuperAdmin {
			reqs = append(reqs, humanRequest(h))
			continue
		}
		if stringInSlice(h.Spec.WorkerAccess, w.Name) {
			reqs = append(reqs, humanRequest(h))
			continue
		}
		if w.Spec.TeamRef != "" {
			for _, entry := range h.Spec.TeamAccess {
				if entry.Team == w.Spec.TeamRef {
					reqs = append(reqs, humanRequest(h))
					break
				}
			}
		}
	}
	return reqs
}

// humanRequest is a small helper to materialise a reconcile.Request.
func humanRequest(h *v1beta1.Human) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKey{Name: h.Name, Namespace: h.Namespace}}
}

// teamRoomsChangedPredicates filters Team events so that only changes
// affecting room membership propagate to Human reconciles.
func teamRoomsChangedPredicates() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldT, ok1 := e.ObjectOld.(*v1beta1.Team)
			newT, ok2 := e.ObjectNew.(*v1beta1.Team)
			if !ok1 || !ok2 {
				return true
			}
			if oldT.Status.TeamRoomID != newT.Status.TeamRoomID ||
				oldT.Status.LeaderDMRoomID != newT.Status.LeaderDMRoomID {
				return true
			}
			// Member list changes also affect computed member-worker-rooms.
			if len(oldT.Status.Members) != len(newT.Status.Members) {
				return true
			}
			return false
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// workerRoomChangedPredicates filters Worker events so Human reconciles
// fire only on room / team / status changes that could alter a Human's
// desired room set.
func workerRoomChangedPredicates() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldW, ok1 := e.ObjectOld.(*v1beta1.Worker)
			newW, ok2 := e.ObjectNew.(*v1beta1.Worker)
			if !ok1 || !ok2 {
				return true
			}
			if oldW.Status.RoomID != newW.Status.RoomID {
				return true
			}
			if oldW.Spec.TeamRef != newW.Spec.TeamRef {
				return true
			}
			if oldW.Spec.Role != newW.Spec.Role {
				return true
			}
			return false
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// stringInSlice is a small helper for slice membership checks.
func stringInSlice(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}
