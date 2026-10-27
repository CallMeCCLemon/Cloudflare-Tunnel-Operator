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
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// TunnelRouteFinalizer guards cleanup of the route's DNS record.
	TunnelRouteFinalizer = "tunnels.latentlab.cc/tunnelroute"

	// ConditionAccepted means the route passed validation and is in the tunnel's config.
	ConditionAccepted = "Accepted"
	// ConditionDNSReady means the hostname's CNAME points at the tunnel.
	ConditionDNSReady = "DNSReady"
)

// TunnelReference names the ClusterTunnel a route attaches to.
type TunnelReference struct {
	// Name of the ClusterTunnel.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ServiceReference is a Service in the route's own namespace.
type ServiceReference struct {
	// Name of the Service.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Port number on the Service.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Scheme cloudflared uses to reach the Service.
	// +kubebuilder:validation:Enum=http;https
	// +kubebuilder:default=http
	// +optional
	Scheme string `json:"scheme,omitempty"`
}

// OriginRequest mirrors the subset of cloudflared's originRequest settings we expose.
type OriginRequest struct {
	// NoTLSVerify disables TLS verification of the origin (for self-signed https Services).
	// +optional
	NoTLSVerify bool `json:"noTLSVerify,omitempty"`

	// HTTP2Origin makes cloudflared speak HTTP/2 to the origin (needed for gRPC; requires https).
	// +optional
	HTTP2Origin bool `json:"http2Origin,omitempty"`

	// HTTPHostHeader overrides the Host header sent to the origin.
	// +optional
	HTTPHostHeader string `json:"httpHostHeader,omitempty"`

	// OriginServerName overrides the TLS SNI hostname sent to the origin.
	// +optional
	OriginServerName string `json:"originServerName,omitempty"`
}

// TunnelRouteSpec defines the desired state of TunnelRoute.
type TunnelRouteSpec struct {
	TunnelRef TunnelReference `json:"tunnelRef"`

	// Hostname to publish, e.g. app.latentlab.cc. Must be in the tunnel's zone.
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`
	Hostname string `json:"hostname"`

	// Path is an optional regex matched against the request path.
	// +optional
	Path string `json:"path,omitempty"`

	Service ServiceReference `json:"service"`

	// +optional
	OriginRequest *OriginRequest `json:"originRequest,omitempty"`

	// AdoptExistingDNSRecord lets the operator take over a CNAME for this hostname
	// that it did not create (for example one made by the dashboard during migration).
	// +optional
	AdoptExistingDNSRecord bool `json:"adoptExistingDnsRecord,omitempty"`
}

// TunnelRouteStatus defines the observed state of TunnelRoute.
type TunnelRouteStatus struct {
	// ServiceURL is the in-cluster URL cloudflared forwards to.
	// +optional
	ServiceURL string `json:"serviceUrl,omitempty"`

	// DNSRecordID is the Cloudflare DNS record managed for this route.
	// +optional
	DNSRecordID string `json:"dnsRecordId,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=troute
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Tunnel",type=string,JSONPath=`.spec.tunnelRef.name`
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.status.serviceUrl`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TunnelRoute publishes a hostname through a ClusterTunnel to a Service in this namespace.
type TunnelRoute struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec TunnelRouteSpec `json:"spec"`

	// +optional
	Status TunnelRouteStatus `json:"status,omitempty"`
}

// ServiceURL is the in-cluster URL for the route's Service.
func (r *TunnelRoute) ServiceURL() string {
	scheme := r.Spec.Service.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", scheme, r.Spec.Service.Name, r.Namespace, r.Spec.Service.Port)
}

// +kubebuilder:object:root=true

// TunnelRouteList contains a list of TunnelRoute
type TunnelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TunnelRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TunnelRoute{}, &TunnelRouteList{})
}
