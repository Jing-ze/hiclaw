package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type workerScope struct {
	worker     *v1beta1.Worker
	provResult *service.WorkerProvisionResult
	patchBase  client.Patch
}

// computePhase determines the Worker status phase based on reconcile outcome.
// When reconcile succeeds, phase reflects the desired lifecycle state.
// When reconcile fails, phase depends on whether infrastructure was provisioned.
func computePhase(w *v1beta1.Worker, reconcileErr error) string {
	if reconcileErr != nil {
		if w.Status.MatrixUserID == "" {
			return "Failed"
		}
		return w.Status.Phase
	}
	return w.Spec.DesiredState()
}
