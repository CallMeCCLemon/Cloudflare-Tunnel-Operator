package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tunnelsv1alpha1 "github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/api/v1alpha1"
	"github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/internal/cloudflare"
)

const (
	operatorNS = "cloudflare-tunnels"
	appNS      = "grpc-demo"
	tunnelName = "home"
	renamedTo  = "latentlab"
)

// harness wires both reconcilers to a fake Kubernetes client and an in-memory
// Cloudflare. Specs call Reconcile directly, in the order they choose; there is
// no manager, cache or watch.
type harness struct {
	ctx    context.Context
	client client.Client
	cf     *cloudflare.Fake
	tunnel *ClusterTunnelReconciler
	route  *TunnelRouteReconciler
}

func newHarness(objs ...client.Object) *harness {
	GinkgoHelper()
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(tunnelsv1alpha1.AddToScheme(scheme)).To(Succeed())

	base := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNS, Labels: map[string]string{"tunnels": "allowed"}}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-api", Namespace: operatorNS},
			Data:       map[string][]byte{"apiToken": []byte("cf-token")},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "kong-proxy", Namespace: appNS},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append(base, objs...)...).
		WithStatusSubresource(&tunnelsv1alpha1.ClusterTunnel{}, &tunnelsv1alpha1.TunnelRoute{}).
		WithIndex(&tunnelsv1alpha1.TunnelRoute{}, indexRouteTunnel, routeTunnelIndex).
		WithIndex(&tunnelsv1alpha1.TunnelRoute{}, indexRouteHostname, routeHostnameIndex).
		WithIndex(&tunnelsv1alpha1.TunnelRoute{}, indexRouteService, routeServiceIndex).
		Build()

	cf := cloudflare.NewFake()
	return &harness{
		ctx:    context.Background(),
		client: c,
		cf:     cf,
		tunnel: &ClusterTunnelReconciler{Client: c, Scheme: scheme, APIReader: c, Cloudflare: cf.Factory()},
		route:  &TunnelRouteReconciler{Client: c, Scheme: scheme, APIReader: c, Cloudflare: cf.Factory()},
	}
}

func newTunnel() *tunnelsv1alpha1.ClusterTunnel {
	return &tunnelsv1alpha1.ClusterTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: tunnelName},
		Spec: tunnelsv1alpha1.ClusterTunnelSpec{
			Cloudflare: tunnelsv1alpha1.CloudflareSpec{
				AccountID: "acct",
				ZoneID:    "zone",
				APITokenSecretRef: tunnelsv1alpha1.SecretKeyReference{
					Name: "cloudflare-api", Namespace: operatorNS, Key: "apiToken",
				},
			},
			Cloudflared: tunnelsv1alpha1.CloudflaredSpec{Namespace: operatorNS},
		},
	}
}

// newRoute sets creationTimestamp explicitly: the fake client doesn't, and
// conflict resolution orders routes by it.
func newRoute(name, hostname string, created time.Time) *tunnelsv1alpha1.TunnelRoute {
	return &tunnelsv1alpha1.TunnelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: appNS,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: tunnelsv1alpha1.TunnelRouteSpec{
			TunnelRef: tunnelsv1alpha1.TunnelReference{Name: tunnelName},
			Hostname:  hostname,
			Service:   tunnelsv1alpha1.ServiceReference{Name: "kong-proxy", Port: 80, Scheme: "http"},
		},
	}
}

func (h *harness) reconcileTunnel() ctrl.Result {
	GinkgoHelper()
	res, err := h.tunnel.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: tunnelName}})
	Expect(err).NotTo(HaveOccurred(), "reconciling ClusterTunnel")
	return res
}

func (h *harness) reconcileRoute(name string) {
	GinkgoHelper()
	key := client.ObjectKey{Namespace: appNS, Name: name}
	_, err := h.route.Reconcile(h.ctx, ctrl.Request{NamespacedName: key})
	Expect(err).NotTo(HaveOccurred(), "reconciling TunnelRoute %s", name)
}

func (h *harness) getTunnel() *tunnelsv1alpha1.ClusterTunnel {
	GinkgoHelper()
	t := &tunnelsv1alpha1.ClusterTunnel{}
	Expect(h.client.Get(h.ctx, client.ObjectKey{Name: tunnelName}, t)).To(Succeed())
	return t
}

func (h *harness) getRoute(name string) *tunnelsv1alpha1.TunnelRoute {
	GinkgoHelper()
	r := &tunnelsv1alpha1.TunnelRoute{}
	Expect(h.client.Get(h.ctx, client.ObjectKey{Namespace: appNS, Name: name}, r)).To(Succeed())
	return r
}

func (h *harness) create(obj client.Object) {
	GinkgoHelper()
	Expect(h.client.Create(h.ctx, obj)).To(Succeed())
}

func (h *harness) update(obj client.Object) {
	GinkgoHelper()
	Expect(h.client.Update(h.ctx, obj)).To(Succeed())
}

func (h *harness) delete(obj client.Object) {
	GinkgoHelper()
	Expect(h.client.Delete(h.ctx, obj)).To(Succeed())
}

// conditionOf returns the named condition, or a zero Condition when it's absent,
// so specs can assert on Status/Reason without nil checks.
func conditionOf(conds []metav1.Condition, condType string) metav1.Condition {
	if c := meta.FindStatusCondition(conds, condType); c != nil {
		return *c
	}
	return metav1.Condition{}
}
