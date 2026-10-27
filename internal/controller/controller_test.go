package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
)

type harness struct {
	t      *testing.T
	ctx    context.Context
	client client.Client
	cf     *cloudflare.Fake
	tunnel *ClusterTunnelReconciler
	route  *TunnelRouteReconciler
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := tunnelsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

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
		t:      t,
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

func newRoute(name, hostname string, created time.Time) *tunnelsv1alpha1.TunnelRoute {
	return &tunnelsv1alpha1.TunnelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: appNS,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: tunnelsv1alpha1.TunnelRouteSpec{
			TunnelRef: tunnelsv1alpha1.TunnelReference{Name: "home"},
			Hostname:  hostname,
			Service:   tunnelsv1alpha1.ServiceReference{Name: "kong-proxy", Port: 80, Scheme: "http"},
		},
	}
}

func (h *harness) reconcileTunnel() {
	h.t.Helper()
	if _, err := h.tunnel.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: tunnelName}}); err != nil {
		h.t.Fatalf("reconciling tunnel: %v", err)
	}
}

func (h *harness) reconcileRoute(name string) {
	h.t.Helper()
	key := client.ObjectKey{Namespace: appNS, Name: name}
	if _, err := h.route.Reconcile(h.ctx, ctrl.Request{NamespacedName: key}); err != nil {
		h.t.Fatalf("reconciling route %s: %v", name, err)
	}
}

func (h *harness) getTunnel() *tunnelsv1alpha1.ClusterTunnel {
	h.t.Helper()
	t := &tunnelsv1alpha1.ClusterTunnel{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Name: tunnelName}, t); err != nil {
		h.t.Fatal(err)
	}
	return t
}

func (h *harness) getRoute(name string) *tunnelsv1alpha1.TunnelRoute {
	h.t.Helper()
	r := &tunnelsv1alpha1.TunnelRoute{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: appNS, Name: name}, r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func condition(conds []metav1.Condition, t string) metav1.Condition {
	c := meta.FindStatusCondition(conds, t)
	if c == nil {
		return metav1.Condition{}
	}
	return *c
}

func TestClusterTunnelCreatesTunnelAndConnectors(t *testing.T) {
	h := newHarness(t, newTunnel())
	h.reconcileTunnel()

	tunnel := h.getTunnel()
	if tunnel.Status.TunnelID == "" || !tunnel.Status.Managed {
		t.Fatalf("expected a managed tunnel, got status %+v", tunnel.Status)
	}
	if want := tunnel.Status.TunnelID + ".cfargotunnel.com"; tunnel.Status.TunnelCNAME != want {
		t.Errorf("TunnelCNAME = %q, want %q", tunnel.Status.TunnelCNAME, want)
	}

	secret := &corev1.Secret{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: operatorNS, Name: "cloudflared-home-token"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[tokenSecretKey]) != "token-for-"+tunnel.Status.TunnelID {
		t.Errorf("token secret = %q", secret.Data[tokenSecretKey])
	}

	deploy := &appsv1.Deployment{}
	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: operatorNS, Name: "cloudflared-home"}, deploy); err != nil {
		t.Fatal(err)
	}
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *deploy.Spec.Replicas)
	}
	if img := deploy.Spec.Template.Spec.Containers[0].Image; img != tunnelsv1alpha1.DefaultCloudflaredImage {
		t.Errorf("image = %q", img)
	}
	if len(deploy.OwnerReferences) != 1 || deploy.OwnerReferences[0].Name != "home" {
		t.Errorf("owner refs = %+v", deploy.OwnerReferences)
	}

	if err := h.client.Get(h.ctx, client.ObjectKey{Namespace: operatorNS, Name: "cloudflared-home"}, &policyv1.PodDisruptionBudget{}); err != nil {
		t.Errorf("PDB: %v", err)
	}

	cfg := h.cf.Configs[tunnel.Status.TunnelID]
	if len(cfg.Ingress) != 1 || cfg.Ingress[0].Service != "http_status:404" {
		t.Errorf("config = %+v, want only the catch-all", cfg)
	}

	// No replicas are ready in the fake cluster, so the tunnel isn't Ready yet.
	if c := condition(tunnel.Status.Conditions, tunnelsv1alpha1.ConditionReady); c.Reason != "Progressing" {
		t.Errorf("Ready = %+v", c)
	}

	// A second pass must not create another tunnel or push an unchanged config.
	pushes := h.cf.ConfigPushes
	h.reconcileTunnel()
	if len(h.cf.Tunnels) != 1 {
		t.Errorf("tunnels = %d, want 1", len(h.cf.Tunnels))
	}
	if h.cf.ConfigPushes != pushes {
		t.Errorf("config pushed again without changes")
	}
}

func TestClusterTunnelAdoptsExistingTunnel(t *testing.T) {
	h := newHarness(t)
	existing, _ := h.cf.CreateTunnel(h.ctx, "acct", "legacy")
	tunnel := newTunnel()
	tunnel.Spec.ExistingTunnelID = existing.ID
	if err := h.client.Create(h.ctx, tunnel); err != nil {
		t.Fatal(err)
	}

	h.reconcileTunnel()
	got := h.getTunnel()
	if got.Status.TunnelID != existing.ID || got.Status.Managed {
		t.Fatalf("status = %+v, want adopted %s", got.Status, existing.ID)
	}

	// Deleting an adopted tunnel leaves it in Cloudflare.
	if err := h.client.Delete(h.ctx, got); err != nil {
		t.Fatal(err)
	}
	h.reconcileTunnel()
	if _, ok := h.cf.Tunnels[existing.ID]; !ok {
		t.Error("adopted tunnel was deleted from Cloudflare")
	}
}

func TestRouteEndToEnd(t *testing.T) {
	h := newHarness(t, newTunnel(), newRoute("web", "grpc.latentlab.cc", time.Now()))
	h.reconcileTunnel()
	h.reconcileRoute("web")

	route := h.getRoute("web")
	if c := condition(route.Status.Conditions, tunnelsv1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("route not ready: %+v", route.Status.Conditions)
	}
	if want := "http://kong-proxy.grpc-demo.svc.cluster.local:80"; route.Status.ServiceURL != want {
		t.Errorf("ServiceURL = %q, want %q", route.Status.ServiceURL, want)
	}

	tunnel := h.getTunnel()
	rec := h.cf.Records[route.Status.DNSRecordID]
	if rec == nil || rec.Type != "CNAME" || rec.Content != tunnel.Status.TunnelCNAME || !rec.Proxied {
		t.Fatalf("DNS record = %+v", rec)
	}

	// The tunnel controller picks up the accepted route.
	h.reconcileTunnel()
	cfg := h.cf.Configs[tunnel.Status.TunnelID]
	if len(cfg.Ingress) != 2 || cfg.Ingress[0].Hostname != "grpc.latentlab.cc" ||
		cfg.Ingress[0].Service != "http://kong-proxy.grpc-demo.svc.cluster.local:80" {
		t.Fatalf("config = %+v", cfg)
	}
	if got := h.getTunnel().Status.Routes; got != 1 {
		t.Errorf("Routes = %d, want 1", got)
	}

	// Deleting the route removes its DNS record and its rule.
	if err := h.client.Delete(h.ctx, route); err != nil {
		t.Fatal(err)
	}
	h.reconcileRoute("web")
	if len(h.cf.Records) != 0 {
		t.Errorf("DNS records left behind: %+v", h.cf.Records)
	}
	if err := h.client.Get(h.ctx, client.ObjectKeyFromObject(route), &tunnelsv1alpha1.TunnelRoute{}); !apierrors.IsNotFound(err) {
		t.Errorf("route still exists after finalizer: %v", err)
	}
	h.reconcileTunnel()
	if cfg := h.cf.Configs[tunnel.Status.TunnelID]; len(cfg.Ingress) != 1 {
		t.Errorf("config after delete = %+v", cfg)
	}
}

func TestRouteHostnameConflict(t *testing.T) {
	now := time.Now()
	h := newHarness(t, newTunnel(),
		newRoute("first", "app.latentlab.cc", now.Add(-time.Hour)),
		newRoute("second", "app.latentlab.cc", now))
	h.reconcileTunnel()
	h.reconcileRoute("second")
	h.reconcileRoute("first")

	if c := condition(h.getRoute("second").Status.Conditions, tunnelsv1alpha1.ConditionAccepted); c.Reason != "HostnameConflict" {
		t.Errorf("second Accepted = %+v, want HostnameConflict", c)
	}
	if c := condition(h.getRoute("first").Status.Conditions, tunnelsv1alpha1.ConditionAccepted); c.Status != metav1.ConditionTrue {
		t.Errorf("first Accepted = %+v", c)
	}

	// Once the older route goes away, the newer one takes over.
	if err := h.client.Delete(h.ctx, h.getRoute("first")); err != nil {
		t.Fatal(err)
	}
	h.reconcileRoute("first")
	h.reconcileRoute("second")
	if c := condition(h.getRoute("second").Status.Conditions, tunnelsv1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Errorf("second Ready = %+v after first was deleted", c)
	}
}

func TestRouteSameHostnameDifferentPaths(t *testing.T) {
	now := time.Now()
	api := newRoute("api", "app.latentlab.cc", now.Add(-time.Hour))
	api.Spec.Path = "^/api/"
	web := newRoute("web", "app.latentlab.cc", now)
	h := newHarness(t, newTunnel(), api, web)
	h.reconcileTunnel()
	h.reconcileRoute("api")
	h.reconcileRoute("web")
	h.reconcileTunnel()

	for _, name := range []string{"api", "web"} {
		if c := condition(h.getRoute(name).Status.Conditions, tunnelsv1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
			t.Errorf("%s Ready = %+v", name, c)
		}
	}
	cfg := h.cf.Configs[h.getTunnel().Status.TunnelID]
	if len(cfg.Ingress) != 3 || cfg.Ingress[0].Path != "^/api/" || cfg.Ingress[1].Path != "" {
		t.Fatalf("path rule must come before bare hostname: %+v", cfg.Ingress)
	}

	// Removing one route keeps the shared DNS record for the other.
	if err := h.client.Delete(h.ctx, h.getRoute("api")); err != nil {
		t.Fatal(err)
	}
	h.reconcileRoute("api")
	if len(h.cf.Records) != 1 {
		t.Errorf("shared DNS record was removed: %+v", h.cf.Records)
	}
}

func TestRouteNamespaceNotAllowed(t *testing.T) {
	tunnel := newTunnel()
	tunnel.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tunnels": "nope"}}
	h := newHarness(t, tunnel, newRoute("web", "app.latentlab.cc", time.Now()))
	h.reconcileTunnel()
	h.reconcileRoute("web")

	if c := condition(h.getRoute("web").Status.Conditions, tunnelsv1alpha1.ConditionAccepted); c.Reason != "NamespaceNotAllowed" {
		t.Errorf("Accepted = %+v", c)
	}
	if len(h.cf.Records) != 0 {
		t.Errorf("DNS created for rejected route")
	}
}

func TestRouteServiceMissing(t *testing.T) {
	route := newRoute("web", "app.latentlab.cc", time.Now())
	route.Spec.Service.Name = "missing"
	h := newHarness(t, newTunnel(), route)
	h.reconcileTunnel()
	h.reconcileRoute("web")

	if c := condition(h.getRoute("web").Status.Conditions, tunnelsv1alpha1.ConditionAccepted); c.Reason != "ServiceNotFound" {
		t.Errorf("Accepted = %+v", c)
	}
}

func TestRouteUnmanagedDNSRecord(t *testing.T) {
	h := newHarness(t, newTunnel(), newRoute("web", "app.latentlab.cc", time.Now()))
	legacy, _ := h.cf.CreateDNSRecord(h.ctx, "zone", cloudflare.DNSRecord{
		Type: "CNAME", Name: "app.latentlab.cc", Content: "old-tunnel.cfargotunnel.com", Proxied: true,
	})
	h.reconcileTunnel()
	h.reconcileRoute("web")

	if c := condition(h.getRoute("web").Status.Conditions, tunnelsv1alpha1.ConditionDNSReady); c.Reason != "DNSRecordConflict" {
		t.Fatalf("DNSReady = %+v, want DNSRecordConflict", c)
	}
	if h.cf.Records[legacy.ID].Content != "old-tunnel.cfargotunnel.com" {
		t.Fatal("unmanaged record was modified")
	}

	// Opting in to adoption repoints the record at the new tunnel.
	route := h.getRoute("web")
	route.Spec.AdoptExistingDNSRecord = true
	if err := h.client.Update(h.ctx, route); err != nil {
		t.Fatal(err)
	}
	h.reconcileRoute("web")
	rec := h.cf.Records[legacy.ID]
	if rec.Content != h.getTunnel().Status.TunnelCNAME || rec.Comment != dnsComment(h.getTunnel()) {
		t.Fatalf("record not adopted: %+v", rec)
	}
}

func TestClusterTunnelDeletionRemovesManagedTunnel(t *testing.T) {
	h := newHarness(t, newTunnel())
	h.reconcileTunnel()
	id := h.getTunnel().Status.TunnelID

	if err := h.client.Delete(h.ctx, h.getTunnel()); err != nil {
		t.Fatal(err)
	}
	h.reconcileTunnel()

	if _, ok := h.cf.Tunnels[id]; ok {
		t.Error("managed tunnel still exists in Cloudflare")
	}
	if err := h.client.Get(h.ctx, client.ObjectKey{Name: "home"}, &tunnelsv1alpha1.ClusterTunnel{}); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterTunnel still exists: %v", err)
	}
}

func TestMissingCredentials(t *testing.T) {
	tunnel := newTunnel()
	tunnel.Spec.Cloudflare.APITokenSecretRef.Name = "does-not-exist"
	h := newHarness(t, tunnel)

	res, err := h.tunnel.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "home"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while credentials are missing")
	}
	if c := condition(h.getTunnel().Status.Conditions, tunnelsv1alpha1.ConditionReady); c.Reason != "CredentialsUnavailable" {
		t.Errorf("Ready = %+v", c)
	}
}

func TestRouteListLagDoesNotDuplicateDNS(t *testing.T) {
	h := newHarness(t, newTunnel(), newRoute("web", "app.latentlab.cc", time.Now()))
	h.reconcileTunnel()
	h.reconcileRoute("web")
	id := h.getRoute("web").Status.DNSRecordID

	// The record exists but the list endpoint hasn't caught up yet.
	h.cf.HiddenFromList[id] = true
	h.reconcileRoute("web")
	if len(h.cf.Records) != 1 {
		t.Fatalf("duplicate DNS record created during list lag: %+v", h.cf.Records)
	}

	if err := h.client.Delete(h.ctx, h.getRoute("web")); err != nil {
		t.Fatal(err)
	}
	h.reconcileRoute("web")
	if len(h.cf.Records) != 0 {
		t.Fatalf("record hidden from list was not deleted: %+v", h.cf.Records)
	}
}

func TestRouteCollapsesAndRemovesDuplicateDNS(t *testing.T) {
	h := newHarness(t, newTunnel(), newRoute("web", "app.latentlab.cc", time.Now()))
	h.reconcileTunnel()
	tunnel := h.getTunnel()
	for range 2 {
		_, _ = h.cf.CreateDNSRecord(h.ctx, "zone", cloudflare.DNSRecord{
			Type: "CNAME", Name: "app.latentlab.cc", Content: tunnel.Status.TunnelCNAME,
			Proxied: true, Comment: dnsComment(tunnel),
		})
	}
	unrelated, _ := h.cf.CreateDNSRecord(h.ctx, "zone", cloudflare.DNSRecord{
		Type: "CNAME", Name: "other.latentlab.cc", Content: "elsewhere.example.com",
	})

	h.reconcileRoute("web")
	if len(h.cf.Records) != 2 {
		t.Fatalf("expected duplicates collapsed to one plus the unrelated record: %+v", h.cf.Records)
	}

	if err := h.client.Delete(h.ctx, h.getRoute("web")); err != nil {
		t.Fatal(err)
	}
	h.reconcileRoute("web")
	if len(h.cf.Records) != 1 || h.cf.Records[unrelated.ID] == nil {
		t.Fatalf("expected only the unrelated record left: %+v", h.cf.Records)
	}
}
