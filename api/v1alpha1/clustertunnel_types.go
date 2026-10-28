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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultCloudflaredImage is used when spec.cloudflared.image is empty.
	DefaultCloudflaredImage = "cloudflare/cloudflared:2026.10.4"

	// ClusterTunnelFinalizer guards cleanup of the Cloudflare tunnel.
	ClusterTunnelFinalizer = "tunnels.latentlab.cc/clustertunnel"

	// Condition types shared by both kinds.
	ConditionReady = "Ready"
)

// SecretKeyReference points at one key of a Secret in a specific namespace.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the Secret.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Key within the Secret's data.
	// +kubebuilder:default=apiToken
	// +optional
	Key string `json:"key,omitempty"`
}

// CloudflareSpec holds the account-level settings used to talk to the Cloudflare API.
type CloudflareSpec struct {
	// AccountID is the Cloudflare account that owns the tunnel.
	// +kubebuilder:validation:MinLength=1
	AccountID string `json:"accountId"`

	// ZoneID is the DNS zone where route hostnames get CNAME records.
	// +kubebuilder:validation:MinLength=1
	ZoneID string `json:"zoneId"`

	// APITokenSecretRef points at a Cloudflare API token with
	// "Account: Cloudflare Tunnel: Edit" and "Zone: DNS: Edit" permissions.
	APITokenSecretRef SecretKeyReference `json:"apiTokenSecretRef"`
}

// CloudflaredSpec configures the connector Deployment the operator runs.
type CloudflaredSpec struct {
	// Namespace the cloudflared Deployment and its token Secret are created in.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Replicas of cloudflared. Each replica is an independent connector to the edge.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image overrides the cloudflared image.
	// +optional
	Image string `json:"image,omitempty"`

	// Resources for the cloudflared container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ClusterTunnelSpec defines the desired state of ClusterTunnel.
type ClusterTunnelSpec struct {
	Cloudflare CloudflareSpec `json:"cloudflare"`

	// TunnelName is the tunnel's name in Cloudflare. Defaults to metadata.name.
	// Changing it renames the tunnel in place; the ID, DNS records and connectors
	// are unaffected. An adopted tunnel (existingTunnelId) keeps its own name
	// unless this is set.
	// +optional
	TunnelName string `json:"tunnelName,omitempty"`

	// ExistingTunnelID adopts a tunnel that already exists instead of creating one.
	// Adopted tunnels are never deleted by the operator.
	// +optional
	ExistingTunnelID string `json:"existingTunnelId,omitempty"`

	// NamespaceSelector limits which namespaces may attach TunnelRoutes.
	// Nil allows every namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	Cloudflared CloudflaredSpec `json:"cloudflared"`
}

// ClusterTunnelStatus defines the observed state of ClusterTunnel.
type ClusterTunnelStatus struct {
	// TunnelID is the Cloudflare tunnel UUID.
	// +optional
	TunnelID string `json:"tunnelId,omitempty"`

	// TunnelName is the tunnel's current name in Cloudflare.
	// +optional
	TunnelName string `json:"tunnelName,omitempty"`

	// TunnelCNAME is the target route hostnames should CNAME to.
	// +optional
	TunnelCNAME string `json:"tunnelCname,omitempty"`

	// Managed is true when the operator created the tunnel (and so may delete it).
	// +optional
	Managed bool `json:"managed,omitempty"`

	// Health is the tunnel status reported by Cloudflare (healthy, degraded, down, inactive).
	// +optional
	Health string `json:"health,omitempty"`

	// ReadyReplicas of the cloudflared Deployment.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Routes is the number of hostnames in the pushed ingress config.
	// Defaulted so the print column shows 0 rather than blank; a merge patch
	// never sends an unchanged zero.
	// +kubebuilder:default=0
	// +optional
	Routes int32 `json:"routes"`

	// ConfigHash of the last ingress configuration pushed to Cloudflare.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// LastConfigPush is when the ingress configuration was last pushed.
	// +optional
	LastConfigPush *metav1.Time `json:"lastConfigPush,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ctun
// +kubebuilder:printcolumn:name="Tunnel ID",type=string,JSONPath=`.status.tunnelId`
// +kubebuilder:printcolumn:name="Cloudflare Name",type=string,JSONPath=`.status.tunnelName`,priority=1
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Routes",type=integer,JSONPath=`.status.routes`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterTunnel is a Cloudflare tunnel plus the cloudflared connectors that serve it.
// Every TunnelRoute that references it is merged into the tunnel's ingress config.
type ClusterTunnel struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ClusterTunnelSpec `json:"spec"`

	// +optional
	Status ClusterTunnelStatus `json:"status,omitempty"`
}

// CloudflareTunnelName is the name to create or look up the tunnel by.
func (t *ClusterTunnel) CloudflareTunnelName() string {
	if t.Spec.TunnelName != "" {
		return t.Spec.TunnelName
	}
	return t.Name
}

// DesiredTunnelName is the name the operator keeps the tunnel at, or "" when it
// should leave the name alone: an adopted tunnel keeps whatever name it already
// has unless spec.tunnelName asks for one.
func (t *ClusterTunnel) DesiredTunnelName() string {
	if t.Spec.TunnelName == "" && !t.Status.Managed {
		return ""
	}
	return t.CloudflareTunnelName()
}

// +kubebuilder:object:root=true

// ClusterTunnelList contains a list of ClusterTunnel
type ClusterTunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterTunnel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterTunnel{}, &ClusterTunnelList{})
}
