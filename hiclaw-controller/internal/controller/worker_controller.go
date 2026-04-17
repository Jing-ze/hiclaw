package controller

import (
	"context"
	"fmt"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
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

const (
	finalizerName       = "hiclaw.io/cleanup"
	reconcileInterval   = 5 * time.Minute
	reconcileRetryDelay = 30 * time.Second
)

// WorkerReconciler reconciles Worker resources using Service-layer orchestration.
type WorkerReconciler struct {
	client.Client

	Provisioner service.WorkerProvisioner
	Deployer    service.WorkerDeployer
	Backend     *backend.Registry
	EnvBuilder  service.WorkerEnvBuilderI
	Legacy      *service.LegacyCompat // nil in incluster mode
}

func (r *WorkerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	logger := log.FromContext(ctx)

	var worker v1beta1.Worker
	if err := r.Get(ctx, req.NamespacedName, &worker); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Sync derived labels (hiclaw.io/team, hiclaw.io/role) from spec. Team
	// and Manager reconcilers list Workers by these labels; keeping them
	// mirrors of spec gives those queries an O(1) indexed path. Any
	// user-supplied value for these label keys is reclaimed by the
	// controller — they are not user-managed metadata.
	if changed, err := r.syncWorkerLabels(ctx, &worker); err != nil {
		return reconcile.Result{}, err
	} else if changed {
		// Labels changed and were persisted via Update; the reconcile will
		// be re-triggered by the write event. Exit now to avoid operating
		// on a stale object copy.
		return reconcile.Result{Requeue: true}, nil
	}

	patchBase := client.MergeFrom(worker.DeepCopy())

	s := &workerScope{
		worker:    &worker,
		patchBase: patchBase,
	}

	// Unified status patch at the end of every reconcile.
	// ObservedGeneration is only written when reconcile succeeds, preventing
	// the infinite-loop bug where a failed status write triggered re-reconcile
	// with Generation != ObservedGeneration.
	defer func() {
		if !worker.DeletionTimestamp.IsZero() {
			return
		}

		worker.Status.Phase = computePhase(&worker, reterr)
		if reterr == nil {
			worker.Status.ObservedGeneration = worker.Generation
			worker.Status.Message = ""
		} else {
			worker.Status.Message = reterr.Error()
		}

		if err := r.Status().Patch(ctx, &worker, patchBase); err != nil {
			logger.Error(err, "failed to patch worker status")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	// Handle deletion
	if !worker.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&worker, finalizerName) {
			return r.reconcileDelete(ctx, s)
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(&worker, finalizerName) {
		controllerutil.AddFinalizer(&worker, finalizerName)
		if err := r.Update(ctx, &worker); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileNormal(ctx, s)
}

// reconcileNormal runs the declarative convergence loop:
//
//   infrastructure -> team membership -> service account -> config
//   -> leader broadcast -> container -> expose -> legacy
//
// Critical-path phases return early on error; non-critical ones log and
// continue. Team membership runs BEFORE config so the latter consumes
// the effective ChannelPolicy. Leader broadcast runs AFTER config so it
// reads the deployer-written AGENTS.md as its baseline.
func (r *WorkerReconciler) reconcileNormal(ctx context.Context, s *workerScope) (reconcile.Result, error) {
	if res, err := r.reconcileInfrastructure(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if res, err := r.reconcileTeamMembership(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if err := r.Provisioner.EnsureServiceAccount(ctx, s.worker.Name); err != nil {
		return reconcile.Result{}, fmt.Errorf("ServiceAccount: %w", err)
	}
	if res, err := r.reconcileConfig(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if res, err := r.reconcileLeaderBroadcast(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if res, err := r.reconcileContainer(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}

	r.reconcileExpose(ctx, s)
	r.reconcileLegacy(ctx, s)

	w := s.worker
	logger := log.FromContext(ctx)
	if w.Status.ObservedGeneration == 0 {
		logger.Info("worker created", "name", w.Name, "roomID", w.Status.RoomID, "role", w.Spec.EffectiveRole(), "team", w.Spec.TeamRef)
	} else if w.Generation != w.Status.ObservedGeneration {
		logger.Info("worker updated", "name", w.Name)
	}

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

// reconcileExpose reconciles port exposure via the gateway. Non-critical.
func (r *WorkerReconciler) reconcileExpose(ctx context.Context, s *workerScope) {
	w := s.worker
	if len(w.Spec.Expose) == 0 && len(w.Status.ExposedPorts) == 0 {
		return
	}
	exposedPorts, err := r.Provisioner.ReconcileExpose(ctx, w.Name, w.Spec.Expose, w.Status.ExposedPorts)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to reconcile exposed ports (non-fatal)")
		return
	}
	w.Status.ExposedPorts = exposedPorts
}

// reconcileLegacy updates the legacy workers registry. Non-critical.
//
// The Manager.groupAllowFrom update is only issued for Workers that the
// Manager talks to directly (standalone and team_leader). Team workers
// are intentionally excluded because they communicate through the leader,
// not through the Manager. This removes the need for the "team-leader"
// annotation branch that existed in the pre-refactor code.
func (r *WorkerReconciler) reconcileLegacy(ctx context.Context, s *workerScope) {
	w := s.worker
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	logger := log.FromContext(ctx)

	role := w.Spec.EffectiveRole()
	isManagerPeer := role == v1beta1.WorkerRoleStandalone || role == v1beta1.WorkerRoleTeamLeader

	if isManagerPeer && s.provResult != nil {
		if err := r.Legacy.UpdateManagerGroupAllowFrom(s.provResult.MatrixUserID, true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom (non-fatal)")
		}
	}

	if err := r.Legacy.UpdateWorkersRegistry(service.WorkerRegistryEntry{
		Name:         w.Name,
		MatrixUserID: r.Provisioner.MatrixUserID(w.Name),
		RoomID:       w.Status.RoomID,
		Runtime:      w.Spec.Runtime,
		Deployment:   "local",
		Skills:       w.Spec.Skills,
		Role:         role,
		TeamID:       nilIfEmpty(w.Spec.TeamRef),
		Image:        nilIfEmpty(w.Spec.Image),
	}); err != nil {
		logger.Error(err, "registry update failed (non-fatal)")
	}
}

// syncWorkerLabels mirrors spec.role / spec.teamRef into the labels
// hiclaw.io/role / hiclaw.io/team. Returns true when labels were patched
// (caller should requeue). Labels on a Worker with no role default to
// "standalone".
func (r *WorkerReconciler) syncWorkerLabels(ctx context.Context, w *v1beta1.Worker) (bool, error) {
	desiredRole := w.Spec.EffectiveRole()
	desiredTeam := w.Spec.TeamRef

	if w.Labels == nil {
		w.Labels = map[string]string{}
	}
	currentRole := w.Labels[v1beta1.LabelRole]
	currentTeam := w.Labels[v1beta1.LabelTeam]

	if currentRole == desiredRole && currentTeam == desiredTeam {
		return false, nil
	}

	patchBase := client.MergeFrom(w.DeepCopy())
	w.Labels[v1beta1.LabelRole] = desiredRole
	if desiredTeam == "" {
		delete(w.Labels, v1beta1.LabelTeam)
	} else {
		w.Labels[v1beta1.LabelTeam] = desiredTeam
	}
	if err := r.Patch(ctx, w, patchBase); err != nil {
		return false, fmt.Errorf("patch worker labels: %w", err)
	}
	return true, nil
}

// teamKey is a small helper shared across worker team phases.
func teamKey(name, namespace string) client.ObjectKey {
	return client.ObjectKey{Name: name, Namespace: namespace}
}

func (r *WorkerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Worker{})

	if r.Backend != nil {
		if wb := r.Backend.DetectWorkerBackend(context.Background()); wb != nil && wb.Name() == "k8s" {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
					workerName := obj.GetLabels()["hiclaw.io/worker"]
					if workerName == "" {
						return nil
					}
					return []reconcile.Request{
						{NamespacedName: client.ObjectKey{
							Name:      workerName,
							Namespace: obj.GetNamespace(),
						}},
					}
				}),
				builder.WithPredicates(podLifecyclePredicates("hiclaw.io/worker")),
			)
		}
	}

	// Watch Team: when the team's leader, rooms, or admins change, every
	// worker in the team may need a reconcile (to refresh effective
	// ChannelPolicy + leader broadcast).
	bldr = bldr.Watches(
		&v1beta1.Team{},
		handler.EnqueueRequestsFromMapFunc(r.teamToWorkersMapper),
		builder.WithPredicates(teamToWorkersPredicates()),
	)

	// Watch Human: teamAccess / workerAccess changes can alter a worker's
	// admin list and therefore its groupAllowFrom.
	bldr = bldr.Watches(
		&v1beta1.Human{},
		handler.EnqueueRequestsFromMapFunc(r.humanToWorkersMapper),
		builder.WithPredicates(humanToWorkersPredicates()),
	)

	return bldr.Complete(r)
}

// teamToWorkersMapper emits reconcile requests for every Worker in the
// changed Team. Worker reconciler consumes Team.status to compute
// effective ChannelPolicy and leader broadcast inputs, so any observable
// change in Team status should fan out to its members.
func (r *WorkerReconciler) teamToWorkersMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	t, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	var list v1beta1.WorkerList
	if err := r.List(ctx, &list,
		client.InNamespace(t.Namespace),
		client.MatchingLabels{v1beta1.LabelTeam: t.Name}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		w := &list.Items[i]
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: w.Name, Namespace: w.Namespace},
		})
	}
	return reqs
}

// teamToWorkersPredicates filters Team events for Worker-relevant changes:
// team-scope spec updates that affect ChannelPolicy / Heartbeat, or any
// status observation that changes leader / members / admins / rooms.
func teamToWorkersPredicates() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldT, ok1 := e.ObjectOld.(*v1beta1.Team)
			newT, ok2 := e.ObjectNew.(*v1beta1.Team)
			if !ok1 || !ok2 {
				return true
			}
			if oldT.Generation != newT.Generation {
				return true
			}
			if oldT.Status.TeamRoomID != newT.Status.TeamRoomID ||
				oldT.Status.LeaderDMRoomID != newT.Status.LeaderDMRoomID {
				return true
			}
			if leaderChanged(oldT.Status.Leader, newT.Status.Leader) {
				return true
			}
			if len(oldT.Status.Members) != len(newT.Status.Members) ||
				len(oldT.Status.Admins) != len(newT.Status.Admins) {
				return true
			}
			return false
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// leaderChanged reports whether two TeamLeaderObservation pointers differ
// in a way the Worker reconciler cares about.
func leaderChanged(a, b *v1beta1.TeamLeaderObservation) bool {
	if (a == nil) != (b == nil) {
		return true
	}
	if a == nil {
		return false
	}
	return a.Name != b.Name || a.MatrixUserID != b.MatrixUserID || a.Ready != b.Ready
}

// humanToWorkersMapper emits reconcile requests for every Worker the
// changed Human's access declarations might affect: all team members of
// every team in its teamAccess, plus every worker named in workerAccess.
// SuperAdmin Humans fan out to every Worker in the namespace.
func (r *WorkerReconciler) humanToWorkersMapper(ctx context.Context, obj client.Object) []reconcile.Request {
	h, ok := obj.(*v1beta1.Human)
	if !ok {
		return nil
	}
	seen := make(map[client.ObjectKey]bool)
	out := make([]reconcile.Request, 0)

	if h.Spec.SuperAdmin {
		var list v1beta1.WorkerList
		if err := r.List(ctx, &list, client.InNamespace(h.Namespace)); err == nil {
			for i := range list.Items {
				k := client.ObjectKey{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace}
				if !seen[k] {
					seen[k] = true
					out = append(out, reconcile.Request{NamespacedName: k})
				}
			}
		}
		return out
	}

	for _, name := range h.Spec.WorkerAccess {
		k := client.ObjectKey{Name: name, Namespace: h.Namespace}
		if !seen[k] {
			seen[k] = true
			out = append(out, reconcile.Request{NamespacedName: k})
		}
	}
	for _, entry := range h.Spec.TeamAccess {
		var list v1beta1.WorkerList
		if err := r.List(ctx, &list,
			client.InNamespace(h.Namespace),
			client.MatchingLabels{v1beta1.LabelTeam: entry.Team}); err != nil {
			continue
		}
		for i := range list.Items {
			k := client.ObjectKey{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace}
			if !seen[k] {
				seen[k] = true
				out = append(out, reconcile.Request{NamespacedName: k})
			}
		}
	}
	return out
}

// humanToWorkersPredicates filters Human events so Worker reconciles
// only fire on changes that could affect the Worker's admin list.
func humanToWorkersPredicates() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldH, ok1 := e.ObjectOld.(*v1beta1.Human)
			newH, ok2 := e.ObjectNew.(*v1beta1.Human)
			if !ok1 || !ok2 {
				return true
			}
			if oldH.Spec.SuperAdmin != newH.Spec.SuperAdmin {
				return true
			}
			if !teamAccessEqual(oldH.Spec.TeamAccess, newH.Spec.TeamAccess) {
				return true
			}
			if !workerAccessEqual(oldH.Spec.WorkerAccess, newH.Spec.WorkerAccess) {
				return true
			}
			if oldH.Status.MatrixUserID != newH.Status.MatrixUserID {
				return true
			}
			return false
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// workerAccessEqual reports whether two workerAccess slices contain the
// same set (order-sensitive for simplicity — events on reorder only
// trigger one extra reconcile cycle).
func workerAccessEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// podLifecyclePredicates filters Pod events to only trigger reconciliation
// on create, delete, or phase transitions (not every status update).
// labelKey is the pod label used to identify which CR owns the pod
// (e.g. "hiclaw.io/worker" or "hiclaw.io/manager").
func podLifecyclePredicates(labelKey string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetLabels()[labelKey] != ""
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return e.Object.GetLabels()[labelKey] != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew.GetLabels()[labelKey] == "" {
				return false
			}
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return true
			}
			return oldPod.Status.Phase != newPod.Status.Phase
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// --- Package-level helpers ---

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
