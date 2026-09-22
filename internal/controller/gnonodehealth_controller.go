/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	monitoringv1alpha1 "github.com/sw360cab/gno-node-operator/api/v1alpha1"
	"github.com/sw360cab/gno-node-operator/internal/gnorpc"
)

// Fallbacks used when the CRD defaults have not been applied, which happens
// for objects created before the field existed or by a client with an older
// schema. The API server normally fills these in.
const (
	defaultInterval       = 30 * time.Second
	defaultTimeout        = 5 * time.Second
	defaultStallThreshold = 5 * time.Minute
)

// prober is the RPC surface the reconciler depends on. Declaring it here rather
// than taking a *gnorpc.Client lets tests substitute a stub without standing up
// an HTTP server.
type prober interface {
	Status(ctx context.Context, endpoint string) (*gnorpc.Status, error)
}

// GnoNodeHealthReconciler reconciles a GnoNodeHealth object.
type GnoNodeHealthReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder recorder.EventRecorder

	// NewProber builds a prober bounded by the given timeout. Defaults to the
	// real gnorpc client when nil.
	NewProber func(timeout time.Duration) prober
}

// +kubebuilder:rbac:groups=monitoring.k8s.gno.land,resources=gnonodehealths,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.k8s.gno.land,resources=gnonodehealths/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=monitoring.k8s.gno.land,resources=gnonodehealths/finalizers,verbs=update
// Services are read to resolve spec.serviceRef into a URL. Without this marker
// the operator works under a developer kubeconfig and fails with Forbidden in
// the cluster, which is the least fun way to discover a missing permission.
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// The modern events API lives in its own API group; granting only groups=""
// leaves the operator silently unable to emit events.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile probes one Gno node and records what it saw.
//
// Unlike a controller that drives the cluster towards a desired state, this one
// observes: the "desired state" is knowledge, and the output is status. The
// resource is re-queued on a timer because the thing being watched -- a chain
// making progress -- is not something the API server can send us an event for.
func (r *GnoNodeHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var node monitoringv1alpha1.GnoNodeHealth
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	interval := durationOrDefault(node.Spec.Interval, defaultInterval)
	timeout := durationOrDefault(node.Spec.Timeout, defaultTimeout)
	stallThreshold := durationOrDefault(node.Spec.StallThreshold, defaultStallThreshold)

	// Snapshot state from before this probe. Stall detection compares against
	// the previous height, and patchStatus compares against the whole previous
	// status to decide whether a write is needed at all.
	previousHeight := node.Status.LatestBlockHeight
	previousStatus := node.Status.DeepCopy()

	endpoint, err := r.resolveEndpoint(ctx, &node)
	if err != nil {
		// No explicit event here: markUnreachable flips the Reachable
		// condition, and setCondition emits on transitions only. Emitting
		// here as well would write a Warning on every single reconcile.
		r.markUnreachable(&node, monitoringv1alpha1.ReasonServiceMissing, err.Error())
		// A missing Service is a user error, not a transient one. Requeue on
		// the normal interval rather than hammering with backoff.
		return ctrl.Result{RequeueAfter: interval}, r.patchStatus(ctx, &node, previousStatus)
	}
	node.Status.Endpoint = endpoint

	status, err := r.probe(ctx, timeout, endpoint)
	node.Status.LastProbeTime = new(metav1.NewTime(time.Now()))

	if err != nil {
		log.V(1).Info("probe failed", "endpoint", endpoint, "error", err)
		// The last known height is deliberately left in place. During an
		// incident "last seen at height 48213, unreachable since 14:02" beats a
		// zeroed field; lastProbeTime says how stale it is.
		r.markUnreachable(&node, monitoringv1alpha1.ReasonProbeFailed, err.Error())
		return ctrl.Result{RequeueAfter: interval}, r.patchStatus(ctx, &node, previousStatus)
	}

	r.applyObservation(&node, status, previousHeight, stallThreshold)

	log.V(1).Info("probe succeeded",
		"endpoint", endpoint, "height", status.Height, "catchingUp", status.CatchingUp)

	return ctrl.Result{RequeueAfter: interval}, r.patchStatus(ctx, &node, previousStatus)
}

// resolveEndpoint turns spec.serviceRef into an http:// URL.
//
// The Service is read rather than assumed so that a typo in serviceRef.name
// surfaces as a condition instead of a DNS failure buried in a probe error,
// and so that a named port is translated to its number.
func (r *GnoNodeHealthReconciler) resolveEndpoint(
	ctx context.Context, node *monitoringv1alpha1.GnoNodeHealth,
) (string, error) {
	ref := node.Spec.ServiceRef

	namespace := ref.Namespace
	if namespace == "" {
		namespace = node.Namespace
	}

	var svc corev1.Service
	key := types.NamespacedName{Name: ref.Name, Namespace: namespace}
	if err := r.Get(ctx, key, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("service %s not found", key)
		}
		return "", fmt.Errorf("reading service %s: %w", key, err)
	}

	port, err := resolvePort(&svc, ref.Port)
	if err != nil {
		return "", err
	}

	// The cluster-local DNS name, which works for a headless Service too: it
	// resolves to the pod IPs backing it.
	return fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, port), nil
}

// resolvePort maps a Service port name or number to its port number.
func resolvePort(svc *corev1.Service, want string) (int32, error) {
	if want == "" {
		if len(svc.Spec.Ports) == 0 {
			return 0, fmt.Errorf("service %s/%s exposes no ports", svc.Namespace, svc.Name)
		}
		return svc.Spec.Ports[0].Port, nil
	}

	for _, p := range svc.Spec.Ports {
		if p.Name == want {
			return p.Port, nil
		}
		if fmt.Sprintf("%d", p.Port) == want {
			return p.Port, nil
		}
	}

	names := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		names = append(names, fmt.Sprintf("%s(%d)", p.Name, p.Port))
	}
	return 0, fmt.Errorf("service %s/%s has no port %q, only %v",
		svc.Namespace, svc.Name, want, names)
}

func (r *GnoNodeHealthReconciler) probe(
	ctx context.Context, timeout time.Duration, endpoint string,
) (*gnorpc.Status, error) {
	newProber := r.NewProber
	if newProber == nil {
		newProber = func(t time.Duration) prober { return gnorpc.New(t) }
	}
	return newProber(timeout).Status(ctx, endpoint)
}

// applyObservation folds a successful probe into status and conditions.
func (r *GnoNodeHealthReconciler) applyObservation(
	node *monitoringv1alpha1.GnoNodeHealth,
	status *gnorpc.Status,
	previousHeight int64,
	stallThreshold time.Duration,
) {
	now := metav1.NewTime(time.Now())

	node.Status.Moniker = status.Moniker
	node.Status.LatestBlockHeight = status.Height
	node.Status.CatchingUp = status.CatchingUp
	if !status.BlockTime.IsZero() {
		node.Status.LatestBlockTime = new(metav1.NewTime(status.BlockTime))
	}

	r.setCondition(node, monitoringv1alpha1.ConditionReachable, metav1.ConditionTrue,
		monitoringv1alpha1.ReasonProbeSucceeded,
		fmt.Sprintf("RPC probe of %s succeeded", node.Status.Endpoint))

	if status.CatchingUp {
		r.setCondition(node, monitoringv1alpha1.ConditionSynced, metav1.ConditionFalse,
			monitoringv1alpha1.ReasonCatchingUp,
			fmt.Sprintf("node is replaying history at height %d", status.Height))
	} else {
		r.setCondition(node, monitoringv1alpha1.ConditionSynced, metav1.ConditionTrue,
			monitoringv1alpha1.ReasonInSync, "node reports it is in sync")
	}

	// Advancing: did the height move since the last observation?
	//
	// The first-observation case MUST be tested before the height comparison.
	// On a fresh resource previousHeight is 0, so any real height looks like an
	// increase and the node would be declared Advancing on a single data point.
	switch {
	case node.Status.LastHeightChangeTime == nil:
		// Nothing to compare against yet. Start the clock and stay Unknown
		// until a second observation exists.
		node.Status.LastHeightChangeTime = &now
		r.setCondition(node, monitoringv1alpha1.ConditionAdvancing, metav1.ConditionUnknown,
			monitoringv1alpha1.ReasonNoObservation,
			fmt.Sprintf("first observation at height %d", status.Height))

	case status.Height > previousHeight:
		node.Status.LastHeightChangeTime = &now
		r.setCondition(node, monitoringv1alpha1.ConditionAdvancing, metav1.ConditionTrue,
			monitoringv1alpha1.ReasonAdvancing,
			fmt.Sprintf("height advanced to %d", status.Height))

	default:
		stalledFor := now.Sub(node.Status.LastHeightChangeTime.Time)
		if stalledFor > stallThreshold {
			r.setCondition(node, monitoringv1alpha1.ConditionAdvancing, metav1.ConditionFalse,
				monitoringv1alpha1.ReasonStalled,
				fmt.Sprintf("height %d unchanged for %s", status.Height, stalledFor.Round(time.Second)))
		} else {
			// Unchanged but still inside the grace window: a node between
			// blocks looks exactly like a halted one for a moment.
			// Deliberately no elapsed time in this message: it would change on
			// every probe, so status would be rewritten every interval even
			// though nothing meaningful happened.
			r.setCondition(node, monitoringv1alpha1.ConditionAdvancing, metav1.ConditionTrue,
				monitoringv1alpha1.ReasonAdvancing,
				fmt.Sprintf("height %d unchanged, within the %s threshold",
					status.Height, stallThreshold))
		}
	}
}

// markUnreachable sets Reachable=False and makes the other conditions Unknown,
// because a node we cannot reach tells us nothing about sync or progress.
func (r *GnoNodeHealthReconciler) markUnreachable(
	node *monitoringv1alpha1.GnoNodeHealth, reason, message string,
) {
	r.setCondition(node, monitoringv1alpha1.ConditionReachable, metav1.ConditionFalse, reason, message)
	r.setCondition(node, monitoringv1alpha1.ConditionSynced, metav1.ConditionUnknown,
		reason, "node is unreachable")
	r.setCondition(node, monitoringv1alpha1.ConditionAdvancing, metav1.ConditionUnknown,
		reason, "node is unreachable")
}

// setCondition updates a condition and emits an Event when its status flips,
// so `kubectl describe` shows a timeline of transitions rather than only the
// current state.
func (r *GnoNodeHealthReconciler) setCondition(
	node *monitoringv1alpha1.GnoNodeHealth,
	condType string, status metav1.ConditionStatus, reason, message string,
) {
	// Capture the previous status BY VALUE. FindStatusCondition returns a
	// pointer into the conditions slice and SetStatusCondition mutates that
	// slice in place, so holding the pointer would mean comparing the updated
	// condition against itself -- and no transition would ever be detected.
	previousStatus, hadPrevious := metav1.ConditionStatus(""), false
	if prev := meta.FindStatusCondition(node.Status.Conditions, condType); prev != nil {
		previousStatus, hadPrevious = prev.Status, true
	}

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})

	// Emit only when the condition actually moves -- including the move from
	// "not set yet" to its first value. A condition that stays False produces
	// one event, not one per reconcile.
	if !hadPrevious || previousStatus != status {
		// Only False is a warning. Unknown is routine at startup, before a
		// second observation exists, and an Unknown caused by a real problem
		// is always accompanied by Reachable=False, which does warn.
		eventType := corev1.EventTypeNormal
		if status == metav1.ConditionFalse {
			eventType = corev1.EventTypeWarning
		}

		// The reason doubles as the event reason: HeightStalled and
		// ProbeFailed say far more than a mechanical Advancing+False, and each
		// one already names its condition unambiguously.
		if !hadPrevious {
			r.event(node, eventType, reason, "Observe",
				"%s is %s: %s", condType, status, message)
		} else {
			r.event(node, eventType, reason, "Transition",
				"%s changed from %s to %s: %s", condType, previousStatus, status, message)
		}
	}
}

func (r *GnoNodeHealthReconciler) event(
	node *monitoringv1alpha1.GnoNodeHealth,
	eventType, reason, action, note string, args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(node, nil, eventType, reason, action, note, args...)
}

// patchStatus writes status back, but only when it changed. An unconditional
// write wakes the watch, which reconciles, which writes status again.
func (r *GnoNodeHealthReconciler) patchStatus(
	ctx context.Context,
	node *monitoringv1alpha1.GnoNodeHealth,
	previous *monitoringv1alpha1.GnoNodeHealthStatus,
) error {
	node.Status.ObservedGeneration = node.Generation

	// previous must be a snapshot taken before this reconcile touched status.
	// Copying it here instead would compare the mutated status against itself
	// and silently never write anything.
	//
	// lastProbeTime moves on every reconcile, so comparing the whole status
	// would always differ. Compare everything else.
	if statusEqualIgnoringProbeTime(previous, &node.Status) {
		return nil
	}

	if err := r.Status().Update(ctx, node); err != nil {
		// NotFound: deleted underneath us. Conflict: a newer version is already
		// in flight and its watch event will trigger another reconcile.
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

func statusEqualIgnoringProbeTime(a, b *monitoringv1alpha1.GnoNodeHealthStatus) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	x.LastProbeTime, y.LastProbeTime = nil, nil
	return reflect.DeepEqual(x, y)
}

func durationOrDefault(d metav1.Duration, fallback time.Duration) time.Duration {
	if d.Duration <= 0 {
		return fallback
	}
	return d.Duration
}

// SetupWithManager registers the controller with the manager.
func (r *GnoNodeHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// GenerationChangedPredicate drops update events whose .metadata.generation
		// is unchanged, which is exactly the shape of our own status writes: the
		// API server bumps generation for spec changes only.
		//
		// Without it every status write enqueues a second reconcile, and so a
		// second RPC probe, for no new information. Creates and deletes still
		// pass, and progress is driven by RequeueAfter rather than by watches.
		For(&monitoringv1alpha1.GnoNodeHealth{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("gnonodehealth").
		Complete(r)
}
