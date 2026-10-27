package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	tunnelsv1alpha1 "github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/api/v1alpha1"
	"github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/internal/cloudflare"
)

const (
	tokenSecretKey      = "token"
	tokenHashAnnotation = "tunnels.latentlab.cc/token-hash"
	metricsPort         = 2000
	cloudflaredUID      = 65532
)

func cloudflaredName(tunnel *tunnelsv1alpha1.ClusterTunnel) string {
	return "cloudflared-" + tunnel.Name
}

func tokenSecretName(tunnel *tunnelsv1alpha1.ClusterTunnel) string {
	return cloudflaredName(tunnel) + "-token"
}

func cloudflaredLabels(tunnel *tunnelsv1alpha1.ClusterTunnel) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/instance":   tunnel.Name,
		"app.kubernetes.io/managed-by": "cloudflare-tunnel-operator",
	}
}

func hashOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// mutateDeployment sets the desired cloudflared Deployment spec on deploy.
// It is written as a mutate func so CreateOrUpdate keeps server-defaulted fields.
func mutateDeployment(deploy *appsv1.Deployment, tunnel *tunnelsv1alpha1.ClusterTunnel, tokenHash string) {
	labels := cloudflaredLabels(tunnel)
	image := tunnel.Spec.Cloudflared.Image
	if image == "" {
		image = tunnelsv1alpha1.DefaultCloudflaredImage
	}
	replicas := tunnel.Spec.Cloudflared.Replicas
	if replicas == nil {
		replicas = ptr.To[int32](2)
	}

	deploy.Labels = labels
	deploy.Spec.Replicas = replicas
	deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	deploy.Spec.Template.Labels = labels
	deploy.Spec.Template.Annotations = map[string]string{tokenHashAnnotation: tokenHash}

	pod := &deploy.Spec.Template.Spec
	pod.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To[int64](cloudflaredUID),
		RunAsGroup:   ptr.To[int64](cloudflaredUID),
		Sysctls: []corev1.Sysctl{{
			// Allows ICMP traffic (ping, traceroute) to resources behind cloudflared.
			Name:  "net.ipv4.ping_group_range",
			Value: "65532 65532",
		}},
	}
	// Spread replicas so one node going down doesn't take the tunnel with it.
	pod.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey:   corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
				},
			}},
		},
	}

	// cloudflared's /ready returns 200 only with an active edge connection.
	probe := func(failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/ready",
				Port: intstr.FromInt32(metricsPort),
			}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			FailureThreshold:    failures,
		}
	}

	pod.Containers = []corev1.Container{{
		Name:  "cloudflared",
		Image: image,
		Args: []string{
			"tunnel", "--no-autoupdate",
			"--loglevel", "info",
			"--metrics", "0.0.0.0:2000",
			"run",
		},
		Env: []corev1.EnvVar{{
			Name: "TUNNEL_TOKEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName(tunnel)},
				Key:                  tokenSecretKey,
			}},
		}},
		Ports:          []corev1.ContainerPort{{Name: "metrics", ContainerPort: metricsPort}},
		Resources:      tunnel.Spec.Cloudflared.Resources,
		LivenessProbe:  probe(3),
		ReadinessProbe: probe(1),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}}
}

// mutatePDB keeps at least one connector up through voluntary disruptions.
func mutatePDB(pdb *policyv1.PodDisruptionBudget, tunnel *tunnelsv1alpha1.ClusterTunnel) {
	labels := cloudflaredLabels(tunnel)
	pdb.Labels = labels
	pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(1))
	pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
}

// routeIsLive reports whether a route should be served: accepted and not being deleted.
func routeIsLive(route *tunnelsv1alpha1.TunnelRoute) bool {
	return route.DeletionTimestamp.IsZero() &&
		meta.IsStatusConditionTrue(route.Status.Conditions, tunnelsv1alpha1.ConditionAccepted)
}

// buildTunnelConfig turns the live routes into a deterministic ingress config.
// Rules with a path sort ahead of the bare hostname so they get a chance to match,
// and a catch-all 404 always comes last, as cloudflared requires.
func buildTunnelConfig(routes []tunnelsv1alpha1.TunnelRoute) cloudflare.TunnelConfig {
	live := make([]tunnelsv1alpha1.TunnelRoute, 0, len(routes))
	for i := range routes {
		if routeIsLive(&routes[i]) {
			live = append(live, routes[i])
		}
	}
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i].Spec, live[j].Spec
		if a.Hostname != b.Hostname {
			return a.Hostname < b.Hostname
		}
		if (a.Path == "") != (b.Path == "") {
			return a.Path != ""
		}
		return a.Path < b.Path
	})

	rules := make([]cloudflare.IngressRule, 0, len(live)+1)
	for i := range live {
		route := &live[i]
		rule := cloudflare.IngressRule{
			Hostname: route.Spec.Hostname,
			Path:     route.Spec.Path,
			Service:  route.ServiceURL(),
		}
		if o := route.Spec.OriginRequest; o != nil {
			rule.OriginRequest = &cloudflare.OriginRequest{
				NoTLSVerify:      o.NoTLSVerify,
				HTTP2Origin:      o.HTTP2Origin,
				HTTPHostHeader:   o.HTTPHostHeader,
				OriginServerName: o.OriginServerName,
			}
		}
		rules = append(rules, rule)
	}
	rules = append(rules, cloudflare.IngressRule{Service: "http_status:404"})
	return cloudflare.TunnelConfig{Ingress: rules}
}
