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
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/sw360cab/gno-node-operator/api/v1alpha1"
	"github.com/sw360cab/gno-node-operator/internal/gnorpc"
)

// stubProber stands in for a Gno node, so tests can dictate exactly what the
// chain appears to be doing without an HTTP server or a real node.
type stubProber struct {
	status *gnorpc.Status
	err    error

	calls     int
	lastEnd   string
	lastTimeo time.Duration
}

func (s *stubProber) Status(_ context.Context, endpoint string) (*gnorpc.Status, error) {
	s.calls++
	s.lastEnd = endpoint
	if s.err != nil {
		return nil, s.err
	}
	return s.status, nil
}

var _ = Describe("GnoNodeHealth Controller", func() {
	const (
		resourceName = "rpc-01"
		namespace    = "default"
		serviceName  = "gnoland-rpc"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: namespace}

	var stub *stubProber

	// reconcilerFor wires the controller to the stub prober.
	reconcilerFor := func(s *stubProber) *GnoNodeHealthReconciler {
		return &GnoNodeHealthReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(50),
			NewProber: func(timeout time.Duration) prober {
				s.lastTimeo = timeout
				return s
			},
		}
	}

	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		result, err := reconcilerFor(stub).Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	fetch := func() *monitoringv1alpha1.GnoNodeHealth {
		GinkgoHelper()
		node := &monitoringv1alpha1.GnoNodeHealth{}
		Expect(k8sClient.Get(ctx, key, node)).To(Succeed())
		return node
	}

	conditionOf := func(condType string) *metav1.Condition {
		GinkgoHelper()
		cond := meta.FindStatusCondition(fetch().Status.Conditions, condType)
		Expect(cond).NotTo(BeNil(), "condition %s was not set", condType)
		return cond
	}

	// createNode writes a GnoNodeHealth; spec tweaks are applied by mutate.
	createNode := func(mutate func(*monitoringv1alpha1.GnoNodeHealthSpec)) {
		GinkgoHelper()
		spec := monitoringv1alpha1.GnoNodeHealthSpec{
			ServiceRef: monitoringv1alpha1.ServiceReference{
				Name: serviceName,
				Port: "rpc-public",
			},
			Interval:       metav1.Duration{Duration: 30 * time.Second},
			Timeout:        metav1.Duration{Duration: 5 * time.Second},
			StallThreshold: metav1.Duration{Duration: 5 * time.Minute},
		}
		if mutate != nil {
			mutate(&spec)
		}
		Expect(k8sClient.Create(ctx, &monitoringv1alpha1.GnoNodeHealth{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
			Spec:       spec,
		})).To(Succeed())
	}

	createService := func(ports ...corev1.ServicePort) {
		GinkgoHelper()
		if len(ports) == 0 {
			ports = []corev1.ServicePort{{Name: "p2p-internal", Port: 26656}, {Name: "rpc-public", Port: 26657}}
		}
		Expect(k8sClient.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: namespace},
			Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: ports},
		})).To(Succeed())
	}

	// seedStatus backdates status so stall detection can be exercised without
	// waiting in real time.
	seedStatus := func(height int64, heightChangedAgo time.Duration) {
		GinkgoHelper()
		node := fetch()
		changed := metav1.NewTime(time.Now().Add(-heightChangedAgo))
		node.Status.LatestBlockHeight = height
		node.Status.LastHeightChangeTime = &changed
		Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
	}

	healthyStatus := func(height int64) *gnorpc.Status {
		return &gnorpc.Status{
			Moniker:    "gnocore-rpc-01",
			Network:    "test13",
			Height:     height,
			BlockTime:  time.Now().Add(-2 * time.Second),
			CatchingUp: false,
		}
	}

	BeforeEach(func() {
		stub = &stubProber{status: healthyStatus(1000)}
	})

	AfterEach(func() {
		node := &monitoringv1alpha1.GnoNodeHealth{}
		if err := k8sClient.Get(ctx, key, node); err == nil {
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		}
		svc := &corev1.Service{}
		svcKey := types.NamespacedName{Name: serviceName, Namespace: namespace}
		if err := k8sClient.Get(ctx, svcKey, svc); err == nil {
			Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
		}
	})

	Context("resolving spec.serviceRef", func() {
		It("reports ServiceNotFound and does not probe when the Service is absent", func() {
			createNode(nil)

			reconcileOnce()

			Expect(stub.calls).To(BeZero(), "should not probe when the endpoint is unknown")

			reachable := conditionOf(monitoringv1alpha1.ConditionReachable)
			Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
			Expect(reachable.Reason).To(Equal(monitoringv1alpha1.ReasonServiceMissing))
			Expect(reachable.Message).To(ContainSubstring("not found"))

			// Nothing is known about a node we cannot even address.
			Expect(conditionOf(monitoringv1alpha1.ConditionSynced).Status).To(Equal(metav1.ConditionUnknown))
			Expect(conditionOf(monitoringv1alpha1.ConditionAdvancing).Status).To(Equal(metav1.ConditionUnknown))
		})

		It("builds the cluster-local URL from the named port", func() {
			createService()
			createNode(nil)

			reconcileOnce()

			want := fmt.Sprintf("http://%s.%s.svc:26657", serviceName, namespace)
			Expect(stub.lastEnd).To(Equal(want))
			Expect(fetch().Status.Endpoint).To(Equal(want))
		})

		It("accepts a numeric port", func() {
			createService()
			createNode(func(s *monitoringv1alpha1.GnoNodeHealthSpec) { s.ServiceRef.Port = "26656" })

			reconcileOnce()

			Expect(stub.lastEnd).To(HaveSuffix(":26656"))
		})

		It("reports a port that the Service does not expose", func() {
			createService(corev1.ServicePort{Name: "p2p-internal", Port: 26656})
			createNode(nil)

			reconcileOnce()

			Expect(stub.calls).To(BeZero())
			reachable := conditionOf(monitoringv1alpha1.ConditionReachable)
			Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
			Expect(reachable.Message).To(ContainSubstring(`no port "rpc-public"`))
		})

		It("passes spec.timeout to the prober", func() {
			createService()
			createNode(func(s *monitoringv1alpha1.GnoNodeHealthSpec) {
				s.Timeout = metav1.Duration{Duration: 3 * time.Second}
			})

			reconcileOnce()

			Expect(stub.lastTimeo).To(Equal(3 * time.Second))
		})
	})

	Context("probing a reachable node", func() {
		BeforeEach(func() { createService() })

		It("records the sync figures and reports Reachable and Synced", func() {
			createNode(nil)

			reconcileOnce()

			node := fetch()
			Expect(node.Status.LatestBlockHeight).To(Equal(int64(1000)))
			Expect(node.Status.Moniker).To(Equal("gnocore-rpc-01"))
			Expect(node.Status.CatchingUp).To(BeFalse())
			Expect(node.Status.LastProbeTime).NotTo(BeNil())
			Expect(node.Status.ObservedGeneration).To(Equal(node.Generation))

			Expect(conditionOf(monitoringv1alpha1.ConditionReachable).Status).To(Equal(metav1.ConditionTrue))
			Expect(conditionOf(monitoringv1alpha1.ConditionSynced).Status).To(Equal(metav1.ConditionTrue))
		})

		It("leaves Advancing Unknown on the very first observation", func() {
			createNode(nil)

			reconcileOnce()

			// One height is not a trend. Claiming True here would mark a node
			// that died before the CR existed as healthy forever.
			advancing := conditionOf(monitoringv1alpha1.ConditionAdvancing)
			Expect(advancing.Status).To(Equal(metav1.ConditionUnknown))
			Expect(advancing.Reason).To(Equal(monitoringv1alpha1.ReasonNoObservation))
			Expect(fetch().Status.LastHeightChangeTime).NotTo(BeNil())
		})

		It("reports Synced=False while the node is catching up", func() {
			stub.status = healthyStatus(1000)
			stub.status.CatchingUp = true
			createNode(nil)

			reconcileOnce()

			synced := conditionOf(monitoringv1alpha1.ConditionSynced)
			Expect(synced.Status).To(Equal(metav1.ConditionFalse))
			Expect(synced.Reason).To(Equal(monitoringv1alpha1.ReasonCatchingUp))
			// Catching up is still reachable.
			Expect(conditionOf(monitoringv1alpha1.ConditionReachable).Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("detecting progress and stalls", func() {
		BeforeEach(func() { createService() })

		It("reports Advancing=True when the height moves", func() {
			createNode(nil)
			seedStatus(999, 30*time.Second)
			stub.status = healthyStatus(1000)

			before := fetch().Status.LastHeightChangeTime

			reconcileOnce()

			advancing := conditionOf(monitoringv1alpha1.ConditionAdvancing)
			Expect(advancing.Status).To(Equal(metav1.ConditionTrue))
			Expect(advancing.Reason).To(Equal(monitoringv1alpha1.ReasonAdvancing))
			// The clock restarts on every increase.
			Expect(fetch().Status.LastHeightChangeTime.Time).To(BeTemporally(">", before.Time))
		})

		It("tolerates an unchanged height inside the stall threshold", func() {
			createNode(nil)
			// A node between blocks looks exactly like a halted one.
			seedStatus(1000, 30*time.Second)
			stub.status = healthyStatus(1000)

			before := fetch().Status.LastHeightChangeTime

			reconcileOnce()

			Expect(conditionOf(monitoringv1alpha1.ConditionAdvancing).Status).To(Equal(metav1.ConditionTrue))
			// The clock must NOT restart, or a stall could never be detected.
			Expect(fetch().Status.LastHeightChangeTime.Time).To(BeTemporally("==", before.Time))
		})

		It("reports Advancing=False once the height is stuck past the threshold", func() {
			createNode(nil)
			seedStatus(1000, 10*time.Minute) // threshold is 5m
			stub.status = healthyStatus(1000)

			reconcileOnce()

			advancing := conditionOf(monitoringv1alpha1.ConditionAdvancing)
			Expect(advancing.Status).To(Equal(metav1.ConditionFalse))
			Expect(advancing.Reason).To(Equal(monitoringv1alpha1.ReasonStalled))
			Expect(advancing.Message).To(ContainSubstring("1000"))

			// The node is up and believes it is in sync -- which is precisely
			// why a liveness probe would report it healthy.
			Expect(conditionOf(monitoringv1alpha1.ConditionReachable).Status).To(Equal(metav1.ConditionTrue))
			Expect(conditionOf(monitoringv1alpha1.ConditionSynced).Status).To(Equal(metav1.ConditionTrue))
		})

		It("recovers to Advancing=True when the chain resumes", func() {
			createNode(nil)
			seedStatus(1000, 10*time.Minute)
			stub.status = healthyStatus(1000)
			reconcileOnce()
			Expect(conditionOf(monitoringv1alpha1.ConditionAdvancing).Status).To(Equal(metav1.ConditionFalse))

			stub.status = healthyStatus(1001)
			reconcileOnce()

			Expect(conditionOf(monitoringv1alpha1.ConditionAdvancing).Status).To(Equal(metav1.ConditionTrue))
		})

		It("honours a custom stallThreshold", func() {
			createNode(func(s *monitoringv1alpha1.GnoNodeHealthSpec) {
				s.StallThreshold = metav1.Duration{Duration: time.Hour}
			})
			seedStatus(1000, 10*time.Minute)
			stub.status = healthyStatus(1000)

			reconcileOnce()

			// 10m stuck is fine when the threshold is an hour.
			Expect(conditionOf(monitoringv1alpha1.ConditionAdvancing).Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Context("when the probe fails", func() {
		BeforeEach(func() { createService() })

		It("keeps the last known height and marks the rest Unknown", func() {
			createNode(nil)
			seedStatus(4821, 30*time.Second)

			stub.err = errors.New("connection refused")

			reconcileOnce()

			node := fetch()
			// "last seen at 4821, unreachable since <lastProbeTime>" beats a
			// zeroed field during an incident.
			Expect(node.Status.LatestBlockHeight).To(Equal(int64(4821)))
			Expect(node.Status.LastProbeTime).NotTo(BeNil())

			reachable := conditionOf(monitoringv1alpha1.ConditionReachable)
			Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
			Expect(reachable.Reason).To(Equal(monitoringv1alpha1.ReasonProbeFailed))
			Expect(reachable.Message).To(ContainSubstring("connection refused"))

			Expect(conditionOf(monitoringv1alpha1.ConditionSynced).Status).To(Equal(metav1.ConditionUnknown))
			Expect(conditionOf(monitoringv1alpha1.ConditionAdvancing).Status).To(Equal(metav1.ConditionUnknown))
		})

		It("does not return an error, so the timer rather than backoff drives retries", func() {
			createNode(nil)
			stub.err = errors.New("i/o timeout")

			result := reconcileOnce() // asserts no error
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		})
	})

	Context("requeue and status hygiene", func() {
		BeforeEach(func() { createService() })

		It("requeues after spec.interval", func() {
			createNode(func(s *monitoringv1alpha1.GnoNodeHealthSpec) {
				s.Interval = metav1.Duration{Duration: 15 * time.Second}
			})

			Expect(reconcileOnce().RequeueAfter).To(Equal(15 * time.Second))
		})

		It("does not write status when nothing changed", func() {
			createNode(nil)
			reconcileOnce()
			reconcileOnce() // settles: Advancing goes Unknown -> True

			settled := fetch().ResourceVersion

			reconcileOnce()

			// lastProbeTime moves every pass and is excluded from the diff, so a
			// steady node must produce no writes at all. Otherwise every write
			// wakes the watch and the loop never goes quiet.
			Expect(fetch().ResourceVersion).To(Equal(settled))
		})

		It("is a no-op when the resource is gone", func() {
			createNode(nil)
			node := fetch()
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())

			// The work queue is not transactional: a request can outlive its object.
			reconcileOnce()

			Expect(stub.calls).To(BeZero())
			err := k8sClient.Get(ctx, key, &monitoringv1alpha1.GnoNodeHealth{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})
})
