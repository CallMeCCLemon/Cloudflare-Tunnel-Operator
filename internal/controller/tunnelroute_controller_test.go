package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelsv1alpha1 "github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/api/v1alpha1"
	"github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/internal/cloudflare"
)

const (
	appHost       = "app.latentlab.cc"
	kongProxyURL  = "http://kong-proxy.grpc-demo.svc.cluster.local:80"
	unrelatedHost = "other.latentlab.cc"
)

var _ = Describe("TunnelRoute controller", func() {
	var h *harness

	Context("for a valid route", func() {
		BeforeEach(func() {
			h = newHarness(newTunnel(), newRoute("web", "grpc.latentlab.cc", time.Now()))
			h.reconcileTunnel()
			h.reconcileRoute("web")
		})

		It("becomes Ready with the in-cluster service URL", func() {
			route := h.getRoute("web")
			Expect(conditionOf(route.Status.Conditions, tunnelsv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
			Expect(route.Status.ServiceURL).To(Equal(kongProxyURL))
		})

		It("creates a proxied CNAME to the tunnel", func() {
			rec := h.cf.Records[h.getRoute("web").Status.DNSRecordID]
			Expect(rec).NotTo(BeNil())
			Expect(rec.Type).To(Equal("CNAME"))
			Expect(rec.Content).To(Equal(h.getTunnel().Status.TunnelCNAME))
			Expect(rec.Proxied).To(BeTrue())
		})

		It("is added to the tunnel's ingress config", func() {
			h.reconcileTunnel()
			cfg := h.cf.Configs[h.getTunnel().Status.TunnelID]
			Expect(cfg.Ingress).To(HaveLen(2))
			Expect(cfg.Ingress[0].Hostname).To(Equal("grpc.latentlab.cc"))
			Expect(cfg.Ingress[0].Service).To(Equal(kongProxyURL))
			Expect(h.getTunnel().Status.Routes).To(BeEquivalentTo(1))
		})

		It("removes its DNS record, finalizer and config rule when deleted", func() {
			h.reconcileTunnel()
			route := h.getRoute("web")
			h.delete(route)
			h.reconcileRoute("web")

			Expect(h.cf.Records).To(BeEmpty())
			err := h.client.Get(h.ctx, client.ObjectKeyFromObject(route), &tunnelsv1alpha1.TunnelRoute{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "finalizer should let the route go")

			h.reconcileTunnel()
			Expect(h.cf.Configs[h.getTunnel().Status.TunnelID].Ingress).To(HaveLen(1))
		})
	})

	Context("when two routes claim the same hostname", func() {
		BeforeEach(func() {
			now := time.Now()
			h = newHarness(newTunnel(),
				newRoute("first", appHost, now.Add(-time.Hour)),
				newRoute("second", appHost, now))
			h.reconcileTunnel()
			h.reconcileRoute("second")
			h.reconcileRoute("first")
		})

		It("accepts the older route and rejects the newer one", func() {
			Expect(conditionOf(h.getRoute("first").Status.Conditions, tunnelsv1alpha1.ConditionAccepted).Status).
				To(Equal(metav1.ConditionTrue))
			Expect(conditionOf(h.getRoute("second").Status.Conditions, tunnelsv1alpha1.ConditionAccepted).Reason).
				To(Equal("HostnameConflict"))
		})

		It("hands the hostname to the newer route once the older one is deleted", func() {
			h.delete(h.getRoute("first"))
			h.reconcileRoute("first")
			h.reconcileRoute("second")
			Expect(conditionOf(h.getRoute("second").Status.Conditions, tunnelsv1alpha1.ConditionReady).Status).
				To(Equal(metav1.ConditionTrue))
		})
	})

	Context("when routes share a hostname with different paths", func() {
		BeforeEach(func() {
			now := time.Now()
			api := newRoute("api", appHost, now.Add(-time.Hour))
			api.Spec.Path = "^/api/"
			h = newHarness(newTunnel(), api, newRoute("web", appHost, now))
			h.reconcileTunnel()
			h.reconcileRoute("api")
			h.reconcileRoute("web")
			h.reconcileTunnel()
		})

		It("accepts both", func() {
			for _, name := range []string{"api", "web"} {
				Expect(conditionOf(h.getRoute(name).Status.Conditions, tunnelsv1alpha1.ConditionReady).Status).
					To(Equal(metav1.ConditionTrue), name)
			}
		})

		It("orders the path rule before the bare hostname", func() {
			cfg := h.cf.Configs[h.getTunnel().Status.TunnelID]
			Expect(cfg.Ingress).To(HaveLen(3))
			Expect(cfg.Ingress[0].Path).To(Equal("^/api/"))
			Expect(cfg.Ingress[1].Path).To(BeEmpty())
		})

		It("keeps the shared DNS record when one route is deleted", func() {
			h.delete(h.getRoute("api"))
			h.reconcileRoute("api")
			Expect(h.cf.Records).To(HaveLen(1))
		})
	})

	Context("validation", func() {
		It("rejects a route from a namespace the tunnel doesn't allow", func() {
			tunnel := newTunnel()
			tunnel.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tunnels": "nope"}}
			h = newHarness(tunnel, newRoute("web", appHost, time.Now()))
			h.reconcileTunnel()
			h.reconcileRoute("web")

			Expect(conditionOf(h.getRoute("web").Status.Conditions, tunnelsv1alpha1.ConditionAccepted).Reason).
				To(Equal("NamespaceNotAllowed"))
			Expect(h.cf.Records).To(BeEmpty(), "no DNS for a rejected route")
		})

		It("rejects a route whose Service doesn't exist", func() {
			route := newRoute("web", appHost, time.Now())
			route.Spec.Service.Name = "missing"
			h = newHarness(newTunnel(), route)
			h.reconcileTunnel()
			h.reconcileRoute("web")

			Expect(conditionOf(h.getRoute("web").Status.Conditions, tunnelsv1alpha1.ConditionAccepted).Reason).
				To(Equal("ServiceNotFound"))
		})
	})

	Context("when the hostname already has a DNS record the operator didn't create", func() {
		var legacyID string

		BeforeEach(func() {
			h = newHarness(newTunnel(), newRoute("web", appHost, time.Now()))
			legacy, err := h.cf.CreateDNSRecord(h.ctx, "zone", cloudflare.DNSRecord{
				Type: "CNAME", Name: appHost, Content: "old-tunnel.cfargotunnel.com", Proxied: true,
			})
			Expect(err).NotTo(HaveOccurred())
			legacyID = legacy.ID
			h.reconcileTunnel()
			h.reconcileRoute("web")
		})

		It("reports DNSRecordConflict and leaves the record untouched", func() {
			Expect(conditionOf(h.getRoute("web").Status.Conditions, tunnelsv1alpha1.ConditionDNSReady).Reason).
				To(Equal("DNSRecordConflict"))
			Expect(h.cf.Records[legacyID].Content).To(Equal("old-tunnel.cfargotunnel.com"))
		})

		It("repoints and tags the record once adoptExistingDnsRecord is set", func() {
			route := h.getRoute("web")
			route.Spec.AdoptExistingDNSRecord = true
			h.update(route)
			h.reconcileRoute("web")

			tunnel := h.getTunnel()
			rec := h.cf.Records[legacyID]
			Expect(rec.Content).To(Equal(tunnel.Status.TunnelCNAME))
			Expect(rec.Comment).To(Equal(dnsComment(tunnel)))
		})
	})

	Context("DNS edge cases", func() {
		It("doesn't create a duplicate while the list endpoint lags behind a create", func() {
			h = newHarness(newTunnel(), newRoute("web", appHost, time.Now()))
			h.reconcileTunnel()
			h.reconcileRoute("web")
			id := h.getRoute("web").Status.DNSRecordID

			h.cf.HiddenFromList[id] = true
			h.reconcileRoute("web")
			Expect(h.cf.Records).To(HaveLen(1), "found by the ID tracked in status")

			h.delete(h.getRoute("web"))
			h.reconcileRoute("web")
			Expect(h.cf.Records).To(BeEmpty(), "a record hidden from the list is still deleted")
		})

		It("collapses duplicate managed records, then removes them all on delete", func() {
			h = newHarness(newTunnel(), newRoute("web", appHost, time.Now()))
			h.reconcileTunnel()
			tunnel := h.getTunnel()
			for range 2 {
				_, err := h.cf.CreateDNSRecord(h.ctx, "zone", cloudflare.DNSRecord{
					Type: "CNAME", Name: appHost, Content: tunnel.Status.TunnelCNAME,
					Proxied: true, Comment: dnsComment(tunnel),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			unrelated, err := h.cf.CreateDNSRecord(h.ctx, "zone", cloudflare.DNSRecord{
				Type: "CNAME", Name: unrelatedHost, Content: "elsewhere.example.com",
			})
			Expect(err).NotTo(HaveOccurred())

			h.reconcileRoute("web")
			Expect(h.cf.Records).To(HaveLen(2), "duplicates collapsed to one, plus the unrelated record")

			h.delete(h.getRoute("web"))
			h.reconcileRoute("web")
			Expect(h.cf.Records).To(HaveLen(1))
			Expect(h.cf.Records).To(HaveKey(unrelated.ID))
		})
	})
})
