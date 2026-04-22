package backend

import (
	"context"
	"errors"
	"io/fs"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

// PodOverlay carries every controller-computed field that ApplyPodTemplate
// must force onto the final Pod. Anything NOT in this struct is either copied
// verbatim from the PodTemplateSpec (the "user wins" side of the merge) or
// left at the zero value.
type PodOverlay struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	Annotations     map[string]string
	OwnerReferences []metav1.OwnerReference

	ServiceAccountName string
	// Container is the agent-container base (Name="worker", Image, Env,
	// WorkingDir, ImagePullPolicy). Resources / VolumeMounts / SecurityContext
	// etc. are layered on top by ApplyPodTemplate itself.
	Container corev1.Container
	// ResourcesOverride, when non-nil, wins over template container.Resources
	// and DefaultResources. This is the per-CreateRequest resource override
	// path.
	ResourcesOverride *corev1.ResourceRequirements
	// DefaultResources is the backend-level fallback used only when neither
	// ResourcesOverride nor template-container.Resources provides a value.
	DefaultResources corev1.ResourceRequirements

	// TokenVolume + TokenVolumeMount are always appended to Pod volumes and
	// the agent container's volumeMounts, regardless of what the template
	// specifies.
	TokenVolume      corev1.Volume
	TokenVolumeMount corev1.VolumeMount

	// HostAliases from CreateRequest.ExtraHosts; appended to any host
	// aliases the template already declared.
	HostAliases []corev1.HostAlias
}

// LoadAgentPodTemplate reads and parses the PodTemplateSpec YAML at path.
// Returns a zero-value PodTemplateSpec (and NEVER panics or returns an error)
// when the file is absent, unreadable, or malformed — a broken template must
// never block Pod creation. Parse failures are logged via
// controller-runtime's logger so they surface in controller logs without
// breaking the create path.
//
// The file is expected to contain the two top-level fields of
// corev1.PodTemplateSpec directly (metadata:, spec:), NOT a full
// apiVersion/kind-wrapped PodTemplate object.
func LoadAgentPodTemplate(ctx context.Context, path string) corev1.PodTemplateSpec {
	logger := log.FromContext(ctx).WithName("agent-pod-template")
	if path == "" {
		return corev1.PodTemplateSpec{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Info("agent pod template file unreadable; falling back to empty template",
				"path", path, "err", err.Error())
		}
		return corev1.PodTemplateSpec{}
	}
	if len(data) == 0 {
		return corev1.PodTemplateSpec{}
	}
	var tmpl corev1.PodTemplateSpec
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		logger.Error(err, "agent pod template parse failed; falling back to empty template",
			"path", path)
		return corev1.PodTemplateSpec{}
	}
	return tmpl
}

// ApplyPodTemplate overlays controller-owned runtime fields from overlay onto
// a deep copy of tmpl, producing a ready-to-submit *corev1.Pod. This function
// is pure (no I/O, no K8s API calls) for ease of testing.
//
// Merge rules (see design doc section 1.2):
//
//   - metadata.Labels: template first, overlay labels overwrite on key collision.
//   - metadata.Annotations: template first, overlay annotations overwrite on key collision.
//   - metadata.OwnerReferences: overlay wins (template's ownerRefs are discarded).
//   - spec.Containers: template containers NOT named "worker" are preserved
//     as sidecars. If template has a container named "worker", its fields
//     serve as a base that overlay.Container's Name/Image/Env/WorkingDir/
//     ImagePullPolicy overwrite (empty overlay fields fall through to template).
//     overlay.TokenVolumeMount is always appended to the agent container's
//     volumeMounts. Resources: overlay.ResourcesOverride wins, else template
//     container.Resources if non-empty, else overlay.DefaultResources.
//   - spec.Volumes: template volumes + overlay.TokenVolume (appended).
//   - spec.ServiceAccountName: overlay wins.
//   - spec.AutomountServiceAccountToken: forced to false.
//   - spec.RestartPolicy: template if set, otherwise "Always".
//   - spec.HostAliases: template first, overlay.HostAliases appended.
//   - everything else in spec: template wins verbatim (nodeSelector,
//     tolerations, affinity, imagePullSecrets, securityContext, topology
//     spread constraints, runtimeClassName, schedulerName, priorityClassName,
//     dnsPolicy, dnsConfig, etc.).
func ApplyPodTemplate(tmpl corev1.PodTemplateSpec, overlay PodOverlay) *corev1.Pod {
	tmplCopy := tmpl.DeepCopy()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        overlay.Name,
			Namespace:   overlay.Namespace,
			Labels:      mergeStringMaps(tmplCopy.ObjectMeta.Labels, overlay.Labels),
			Annotations: mergeStringMaps(tmplCopy.ObjectMeta.Annotations, overlay.Annotations),
		},
		Spec: tmplCopy.Spec,
	}
	if len(overlay.OwnerReferences) > 0 {
		pod.OwnerReferences = append([]metav1.OwnerReference(nil), overlay.OwnerReferences...)
	}

	agentContainer, sidecars := splitAgentContainer(pod.Spec.Containers, overlay.Container.Name)
	agentContainer = overlayAgentContainer(agentContainer, overlay)
	pod.Spec.Containers = append([]corev1.Container{agentContainer}, sidecars...)

	pod.Spec.Volumes = append(pod.Spec.Volumes, overlay.TokenVolume)

	pod.Spec.ServiceAccountName = overlay.ServiceAccountName
	pod.Spec.AutomountServiceAccountToken = boolPtr(false)

	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	}

	if len(overlay.HostAliases) > 0 {
		pod.Spec.HostAliases = append(pod.Spec.HostAliases, overlay.HostAliases...)
	}

	return pod
}

// splitAgentContainer locates the agent container (by name) within tmpl and
// returns (base, sidecars). When no match exists, returns (zero, tmpl).
func splitAgentContainer(containers []corev1.Container, agentName string) (corev1.Container, []corev1.Container) {
	if agentName == "" {
		agentName = "worker"
	}
	sidecars := make([]corev1.Container, 0, len(containers))
	var base corev1.Container
	found := false
	for _, c := range containers {
		if !found && c.Name == agentName {
			base = c
			found = true
			continue
		}
		sidecars = append(sidecars, c)
	}
	return base, sidecars
}

// overlayAgentContainer merges overlay runtime fields onto base (which may be
// the zero Container when template defined no agent container) and returns
// the final agent container. Resources are resolved per the documented
// precedence (overlay override > template > backend default).
func overlayAgentContainer(base corev1.Container, overlay PodOverlay) corev1.Container {
	out := base
	if out.Name == "" {
		out.Name = overlay.Container.Name
	}
	if overlay.Container.Image != "" {
		out.Image = overlay.Container.Image
	}
	if overlay.Container.ImagePullPolicy != "" {
		out.ImagePullPolicy = overlay.Container.ImagePullPolicy
	} else if out.ImagePullPolicy == "" {
		out.ImagePullPolicy = corev1.PullIfNotPresent
	}
	if len(overlay.Container.Env) > 0 {
		out.Env = overlay.Container.Env
	}
	if overlay.Container.WorkingDir != "" {
		out.WorkingDir = overlay.Container.WorkingDir
	}
	out.VolumeMounts = append(out.VolumeMounts, overlay.TokenVolumeMount)

	switch {
	case overlay.ResourcesOverride != nil:
		out.Resources = *overlay.ResourcesOverride
	case isResourcesEmpty(out.Resources):
		out.Resources = overlay.DefaultResources
	}

	return out
}

func isResourcesEmpty(r corev1.ResourceRequirements) bool {
	return len(r.Limits) == 0 && len(r.Requests) == 0 && len(r.Claims) == 0
}

// mergeStringMaps returns base + overrides with overrides winning on
// key collision. A new map is always returned; inputs are not mutated.
func mergeStringMaps(base, overrides map[string]string) map[string]string {
	if len(base) == 0 && len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
