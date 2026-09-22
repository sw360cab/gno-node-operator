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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types reported by this controller.
const (
	// ConditionReachable is True when the node answered an RPC probe.
	ConditionReachable = "Reachable"

	// ConditionSynced is True when the node reports catching_up == false.
	ConditionSynced = "Synced"

	// ConditionAdvancing is True when the chain height moved within
	// spec.stallThreshold.
	//
	// This is the condition a Kubernetes liveness probe cannot give you: a
	// halted chain still answers RPC and still reports itself in sync. Telling
	// the two apart requires remembering the previous height, which is why
	// this is an operator and not a probe.
	ConditionAdvancing = "Advancing"
)

// Condition reasons.
const (
	ReasonProbeSucceeded = "ProbeSucceeded"
	ReasonProbeFailed    = "ProbeFailed"
	ReasonServiceMissing = "ServiceNotFound"
	ReasonInSync         = "InSync"
	ReasonCatchingUp     = "CatchingUp"
	ReasonAdvancing      = "Advancing"
	ReasonStalled        = "HeightStalled"
	ReasonNoObservation  = "AwaitingObservation"
)

// ServiceReference points at the Service fronting a Gno node's RPC listener.
//
// In a standard gno.land deployment only nodes started with
// `rpc.laddr = tcp://0.0.0.0:26657` are reachable this way. Validators bind RPC
// to loopback on purpose, so they are observed indirectly through their sentries.
type ServiceReference struct {
	// name of the Service fronting the node's RPC port.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// namespace of the Service. Defaults to the namespace of this resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// port is the name or decimal number of the Service port carrying RPC.
	// +kubebuilder:default="rpc-public"
	// +optional
	Port string `json:"port,omitempty"`
}

// GnoNodeHealthSpec defines which node to probe and what "healthy" means for it.
type GnoNodeHealthSpec struct {
	// serviceRef identifies the node to probe.
	// +required
	ServiceRef ServiceReference `json:"serviceRef"`

	// interval is how often the node is probed.
	// +kubebuilder:default="30s"
	// +optional
	Interval metav1.Duration `json:"interval,omitempty"`

	// timeout bounds a single RPC probe.
	// +kubebuilder:default="5s"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty"`

	// stallThreshold is how long the latest block height may stay unchanged
	// before the node is reported as stalled. Set it to a comfortable multiple
	// of the chain's block time.
	// +kubebuilder:default="5m"
	// +optional
	StallThreshold metav1.Duration `json:"stallThreshold,omitempty"`
}

// GnoNodeHealthStatus is the observed state of the node.
type GnoNodeHealthStatus struct {
	// observedGeneration is the .metadata.generation last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// endpoint is the URL the controller resolved from spec.serviceRef.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// moniker is the node's self-reported name.
	// +optional
	Moniker string `json:"moniker,omitempty"`

	// latestBlockHeight as last reported by the node.
	// +optional
	LatestBlockHeight int64 `json:"latestBlockHeight,omitempty"`

	// latestBlockTime as last reported by the node.
	// +optional
	LatestBlockTime *metav1.Time `json:"latestBlockTime,omitempty"`

	// catchingUp mirrors sync_info.catching_up.
	// +optional
	CatchingUp bool `json:"catchingUp,omitempty"`

	// lastProbeTime is when the node was last contacted, successfully or not.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// lastHeightChangeTime is when latestBlockHeight was last seen to increase.
	// Stall detection is `now - lastHeightChangeTime > stallThreshold`, which is
	// the only reason this controller needs to remember anything at all.
	// +optional
	LastHeightChangeTime *metav1.Time `json:"lastHeightChangeTime,omitempty"`

	// conditions holds Reachable, Synced and Advancing.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gnh
// +kubebuilder:printcolumn:name="Height",type=integer,JSONPath=`.status.latestBlockHeight`
// +kubebuilder:printcolumn:name="Reachable",type=string,JSONPath=`.status.conditions[?(@.type=="Reachable")].status`
// +kubebuilder:printcolumn:name="Synced",type=string,JSONPath=`.status.conditions[?(@.type=="Synced")].status`
// +kubebuilder:printcolumn:name="Advancing",type=string,JSONPath=`.status.conditions[?(@.type=="Advancing")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GnoNodeHealth tracks the liveness and sync state of a single Gno node.
type GnoNodeHealth struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of GnoNodeHealth
	// +required
	Spec GnoNodeHealthSpec `json:"spec"`

	// status defines the observed state of GnoNodeHealth
	// +optional
	Status GnoNodeHealthStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// GnoNodeHealthList contains a list of GnoNodeHealth.
type GnoNodeHealthList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []GnoNodeHealth `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &GnoNodeHealth{}, &GnoNodeHealthList{})
		return nil
	})
}
