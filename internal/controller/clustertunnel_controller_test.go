package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelsv1alpha1 "github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/api/v1alpha1"
)

var _ = Describe("ClusterTunnel controller", func() {
	var h *harness

	Context("for a new tunnel", func() {
		BeforeEach(func() {
			h = newHarness(newTunnel())
			h.reconcileTunnel()
		})

		It("creates a managed tunnel in Cloudflare", func() {
			tunnel := h.getTunnel()
			Expect(tunnel.Status.TunnelID).NotTo(BeEmpty())
			Expect(tunnel.Status.Managed).To(BeTrue())
			Expect(tunnel.Status.TunnelName).To(Equal(tunnelName))
			Expect(tunnel.Status.TunnelCNAME).To(Equal(tunnel.Status.TunnelID + ".cfargotunnel.com"))
		})

		It("stores the run token in an owned Secret", func() {
			id := h.getTunnel().Status.TunnelID
			secret := &corev1.Secret{}
			Expect(h.client.Get(h.ctx, client.ObjectKey{Namespace: operatorNS, Name: "cloudflared-home-token"}, secret)).To(Succeed())
			Expect(string(secret.Data[tokenSecretKey])).To(Equal("token-for-" + id))
		})

		It("runs 2 pinned cloudflared replicas with a PodDisruptionBudget", func() {
			deploy := &appsv1.Deployment{}
			key := client.ObjectKey{Namespace: operatorNS, Name: "cloudflared-home"}
			Expect(h.client.Get(h.ctx, key, deploy)).To(Succeed())
			Expect(*deploy.Spec.Replicas).To(BeEquivalentTo(2))
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(tunnelsv1alpha1.DefaultCloudflaredImage))
			Expect(deploy.OwnerReferences).To(HaveLen(1))
			Expect(deploy.OwnerReferences[0].Name).To(Equal(tunnelName))

			Expect(h.client.Get(h.ctx, key, &policyv1.PodDisruptionBudget{})).To(Succeed())
		})

		It("pushes a config with only the catch-all rule", func() {
			cfg := h.cf.Configs[h.getTunnel().Status.TunnelID]
			Expect(cfg.Ingress).To(HaveLen(1))
			Expect(cfg.Ingress[0].Service).To(Equal("http_status:404"))
		})

		It("reports Progressing until cloudflared replicas are ready", func() {
			// The fake cluster never runs pods, so replicas never become ready.
			ready := conditionOf(h.getTunnel().Status.Conditions, tunnelsv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("Progressing"))
		})

		It("is idempotent: no second tunnel and no unchanged config push", func() {
			pushes := h.cf.ConfigPushes
			h.reconcileTunnel()
			Expect(h.cf.Tunnels).To(HaveLen(1))
			Expect(h.cf.ConfigPushes).To(Equal(pushes))
		})

		It("deletes the tunnel from Cloudflare when the ClusterTunnel is deleted", func() {
			id := h.getTunnel().Status.TunnelID
			h.delete(h.getTunnel())
			h.reconcileTunnel()

			Expect(h.cf.Tunnels).NotTo(HaveKey(id))
			err := h.client.Get(h.ctx, client.ObjectKey{Name: tunnelName}, &tunnelsv1alpha1.ClusterTunnel{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "finalizer should let the object go")
		})
	})

	Context("renaming", func() {
		var id string

		BeforeEach(func() {
			h = newHarness(newTunnel())
			h.reconcileTunnel()
			id = h.getTunnel().Status.TunnelID

			tunnel := h.getTunnel()
			tunnel.Spec.TunnelName = renamedTo
			h.update(tunnel)
			h.reconcileTunnel()
		})

		It("renames the same tunnel in place", func() {
			tunnel := h.getTunnel()
			Expect(tunnel.Status.TunnelID).To(Equal(id))
			Expect(h.cf.Tunnels).To(HaveLen(1))
			Expect(h.cf.Tunnels[id].Name).To(Equal(renamedTo))
			Expect(tunnel.Status.TunnelName).To(Equal(renamedTo))
		})

		It("reverts a rename made in the dashboard, and leaves a matching name alone", func() {
			h.cf.Tunnels[id].Name = "renamed-in-dashboard"
			h.reconcileTunnel()
			h.reconcileTunnel()
			Expect(h.cf.Tunnels[id].Name).To(Equal(renamedTo))
			Expect(h.cf.Renames).To(Equal(2), "one rename from the spec change, one to revert the dashboard edit")
		})

		It("goes back to metadata.name when tunnelName is cleared", func() {
			tunnel := h.getTunnel()
			tunnel.Spec.TunnelName = ""
			h.update(tunnel)
			h.reconcileTunnel()
			Expect(h.cf.Tunnels[id].Name).To(Equal(tunnelName))
		})
	})

	Context("adopting an existing tunnel", func() {
		var existingID string

		BeforeEach(func() {
			h = newHarness()
			existing, err := h.cf.CreateTunnel(h.ctx, "acct", "made-in-dashboard")
			Expect(err).NotTo(HaveOccurred())
			existingID = existing.ID

			tunnel := newTunnel()
			tunnel.Spec.ExistingTunnelID = existingID
			h.create(tunnel)
			h.reconcileTunnel()
		})

		It("uses the tunnel without marking it managed", func() {
			tunnel := h.getTunnel()
			Expect(tunnel.Status.TunnelID).To(Equal(existingID))
			Expect(tunnel.Status.Managed).To(BeFalse())
		})

		It("keeps the tunnel's own name unless tunnelName is set", func() {
			Expect(h.cf.Renames).To(BeZero())
			Expect(h.getTunnel().Status.TunnelName).To(Equal("made-in-dashboard"))

			tunnel := h.getTunnel()
			tunnel.Spec.TunnelName = renamedTo
			h.update(tunnel)
			h.reconcileTunnel()
			Expect(h.cf.Tunnels[existingID].Name).To(Equal(renamedTo))
		})

		It("leaves the tunnel in Cloudflare when the ClusterTunnel is deleted", func() {
			h.delete(h.getTunnel())
			h.reconcileTunnel()
			Expect(h.cf.Tunnels).To(HaveKey(existingID))
		})
	})

	Context("when the API token Secret is missing", func() {
		It("reports CredentialsUnavailable and requeues instead of failing", func() {
			tunnel := newTunnel()
			tunnel.Spec.Cloudflare.APITokenSecretRef.Name = "does-not-exist"
			h = newHarness(tunnel)

			res := h.reconcileTunnel()
			Expect(res.RequeueAfter).NotTo(BeZero())
			ready := conditionOf(h.getTunnel().Status.Conditions, tunnelsv1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal("CredentialsUnavailable"))
		})
	})
})
