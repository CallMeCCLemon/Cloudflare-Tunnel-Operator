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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tunnelsv1alpha1 "github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/api/v1alpha1"
	"github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/internal/cloudflare"
)

// DefaultResyncInterval is how often tunnels are re-checked and their config re-pushed,
// which also overwrites any edits made in the Cloudflare dashboard.
const DefaultResyncInterval = 10 * time.Minute

// ClusterTunnelReconciler owns a Cloudflare tunnel, its cloudflared Deployment, and its
// ingress config. It is the only writer of the config, so routes never race each other.
type ClusterTunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads the credentials Secret without caching it.
	APIReader client.Reader
	// Cloudflare builds API clients; tests substitute a fake.
	Cloudflare cloudflare.Factory
	// ResyncInterval defaults to DefaultResyncInterval.
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=clustertunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=clustertunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=clustertunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=tunnelroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ClusterTunnelReconciler) resync() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return DefaultResyncInterval
}

func (r *ClusterTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	tunnel := &tunnelsv1alpha1.ClusterTunnel{}
	if err := r.Get(ctx, req.NamespacedName, tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clients := cloudflareClients{Reader: r.APIReader, Factory: r.Cloudflare}
	cf, credErr := clients.forTunnel(ctx, tunnel)

	if !tunnel.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, tunnel, cf, credErr)
	}

	if controllerutil.AddFinalizer(tunnel, tunnelsv1alpha1.ClusterTunnelFinalizer) {
		if err := r.Update(ctx, tunnel); err != nil {
			return ctrl.Result{}, err
		}
	}
	base := tunnel.DeepCopy()

	var result ctrl.Result
	var reconcileErr error
	if credErr != nil {
		setReady(&tunnel.Status.Conditions, tunnel.Generation, metav1.ConditionFalse, "CredentialsUnavailable", credErr.Error())
		result = ctrl.Result{RequeueAfter: time.Minute}
	} else {
		result, reconcileErr = r.reconcileTunnel(ctx, tunnel, cf)
		if reconcileErr != nil {
			setReady(&tunnel.Status.Conditions, tunnel.Generation, metav1.ConditionFalse, "ReconcileError", reconcileErr.Error())
		}
	}

	tunnel.Status.ObservedGeneration = tunnel.Generation
	if err := r.Status().Patch(ctx, tunnel, client.MergeFrom(base)); err != nil {
		log.Error(err, "patching ClusterTunnel status")
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return result, reconcileErr
}

func (r *ClusterTunnelReconciler) reconcileTunnel(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, cf cloudflare.API) (ctrl.Result, error) {
	accountID := tunnel.Spec.Cloudflare.AccountID

	if err := r.ensureTunnelID(ctx, tunnel, cf); err != nil {
		return ctrl.Result{}, err
	}

	remote, err := cf.GetTunnel(ctx, accountID, tunnel.Status.TunnelID)
	if err != nil {
		if cloudflare.IsNotFound(err) && tunnel.Status.Managed {
			// Someone deleted our tunnel out from under us; create a fresh one next pass.
			tunnel.Status.TunnelID = ""
			tunnel.Status.TunnelCNAME = ""
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching tunnel %s: %w", tunnel.Status.TunnelID, err)
	}
	tunnel.Status.Health = remote.Status

	token, err := cf.GetTunnelToken(ctx, accountID, tunnel.Status.TunnelID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("fetching tunnel token: %w", err)
	}
	if err := r.ensureTokenSecret(ctx, tunnel, token); err != nil {
		return ctrl.Result{}, err
	}

	deploy, err := r.ensureDeployment(ctx, tunnel, hashOf(token))
	if err != nil {
		return ctrl.Result{}, err
	}
	tunnel.Status.ReadyReplicas = deploy.Status.ReadyReplicas

	if err := r.ensurePDB(ctx, tunnel); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.pushConfig(ctx, tunnel, cf); err != nil {
		return ctrl.Result{}, err
	}

	if tunnel.Status.ReadyReplicas > 0 {
		setReady(&tunnel.Status.Conditions, tunnel.Generation, metav1.ConditionTrue, "Available",
			fmt.Sprintf("%d cloudflared replica(s) ready", tunnel.Status.ReadyReplicas))
	} else {
		setReady(&tunnel.Status.Conditions, tunnel.Generation, metav1.ConditionFalse, "Progressing",
			"waiting for cloudflared replicas to become ready")
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// ensureTunnelID resolves the Cloudflare tunnel: adopted by ID, found by name, or created.
func (r *ClusterTunnelReconciler) ensureTunnelID(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, cf cloudflare.API) error {
	accountID := tunnel.Spec.Cloudflare.AccountID

	if id := tunnel.Spec.ExistingTunnelID; id != "" {
		if tunnel.Status.TunnelID != id {
			tunnel.Status.TunnelID = id
			tunnel.Status.Managed = false
		}
	} else if tunnel.Status.TunnelID == "" {
		name := tunnel.CloudflareTunnelName()
		existing, err := cf.FindTunnelByName(ctx, accountID, name)
		if err != nil {
			return fmt.Errorf("looking up tunnel %q: %w", name, err)
		}
		if existing != nil {
			// We can't prove we created it, so treat it as adopted and never delete it.
			tunnel.Status.TunnelID = existing.ID
			tunnel.Status.Managed = false
		} else {
			created, err := cf.CreateTunnel(ctx, accountID, name)
			if err != nil {
				return fmt.Errorf("creating tunnel %q: %w", name, err)
			}
			logf.FromContext(ctx).Info("created Cloudflare tunnel", "tunnel", name, "id", created.ID)
			tunnel.Status.TunnelID = created.ID
			tunnel.Status.Managed = true
		}
	}
	tunnel.Status.TunnelCNAME = tunnel.Status.TunnelID + ".cfargotunnel.com"
	return nil
}

func (r *ClusterTunnelReconciler) ensureTokenSecret(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, token string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      tokenSecretName(tunnel),
		Namespace: tunnel.Spec.Cloudflared.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = cloudflaredLabels(tunnel)
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{tokenSecretKey: []byte(token)}
		return controllerutil.SetControllerReference(tunnel, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconciling token secret: %w", err)
	}
	return nil
}

func (r *ClusterTunnelReconciler) ensureDeployment(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, tokenHash string) (*appsv1.Deployment, error) {
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      cloudflaredName(tunnel),
		Namespace: tunnel.Spec.Cloudflared.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		mutateDeployment(deploy, tunnel, tokenHash)
		return controllerutil.SetControllerReference(tunnel, deploy, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("reconciling cloudflared deployment: %w", err)
	}
	return deploy, nil
}

func (r *ClusterTunnelReconciler) ensurePDB(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel) error {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name:      cloudflaredName(tunnel),
		Namespace: tunnel.Spec.Cloudflared.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		mutatePDB(pdb, tunnel)
		return controllerutil.SetControllerReference(tunnel, pdb, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("reconciling pod disruption budget: %w", err)
	}
	return nil
}

// pushConfig sends the merged ingress config when it changed, or on every resync
// so dashboard edits are reverted.
func (r *ClusterTunnelReconciler) pushConfig(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, cf cloudflare.API) error {
	routes := &tunnelsv1alpha1.TunnelRouteList{}
	if err := r.List(ctx, routes, client.MatchingFields{indexRouteTunnel: tunnel.Name}); err != nil {
		return fmt.Errorf("listing routes: %w", err)
	}
	cfg := buildTunnelConfig(routes.Items)
	hash := hashOf(cfg)

	stale := tunnel.Status.LastConfigPush == nil ||
		time.Since(tunnel.Status.LastConfigPush.Time) >= r.resync()
	if hash != tunnel.Status.ConfigHash || stale {
		if err := cf.UpdateTunnelConfig(ctx, tunnel.Spec.Cloudflare.AccountID, tunnel.Status.TunnelID, cfg); err != nil {
			return fmt.Errorf("pushing tunnel config: %w", err)
		}
		now := metav1.Now()
		tunnel.Status.ConfigHash = hash
		tunnel.Status.LastConfigPush = &now
	}
	tunnel.Status.Routes = int32(len(cfg.Ingress) - 1)
	return nil
}

// finalize removes the tunnel from Cloudflare when the operator created it.
// The cloudflared Deployment is deleted first so the tunnel has no live connections.
func (r *ClusterTunnelReconciler) finalize(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, cf cloudflare.API, credErr error) error {
	if !controllerutil.ContainsFinalizer(tunnel, tunnelsv1alpha1.ClusterTunnelFinalizer) {
		return nil
	}
	log := logf.FromContext(ctx)

	if tunnel.Status.Managed && tunnel.Status.TunnelID != "" {
		if credErr != nil {
			return fmt.Errorf("cannot delete tunnel without credentials: %w", credErr)
		}
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      cloudflaredName(tunnel),
			Namespace: tunnel.Spec.Cloudflared.Namespace,
		}}
		if err := r.Delete(ctx, deploy); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting cloudflared deployment: %w", err)
		}

		accountID := tunnel.Spec.Cloudflare.AccountID
		if err := cf.CleanupTunnelConnections(ctx, accountID, tunnel.Status.TunnelID); err != nil && !cloudflare.IsNotFound(err) {
			return fmt.Errorf("cleaning up tunnel connections: %w", err)
		}
		if err := cf.DeleteTunnel(ctx, accountID, tunnel.Status.TunnelID); err != nil && !cloudflare.IsNotFound(err) {
			// Connectors may still be shutting down; the error requeues with backoff.
			return fmt.Errorf("deleting tunnel %s: %w", tunnel.Status.TunnelID, err)
		}
		log.Info("deleted Cloudflare tunnel", "id", tunnel.Status.TunnelID)
	}

	return removeFinalizer(ctx, r.Client, tunnel, tunnelsv1alpha1.ClusterTunnelFinalizer)
}

func setReady(conds *[]metav1.Condition, gen int64, status metav1.ConditionStatus, reason, msg string) {
	setCondition(conds, gen, tunnelsv1alpha1.ConditionReady, status, reason, msg)
}

func setCondition(conds *[]metav1.Condition, gen int64, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gen,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelsv1alpha1.ClusterTunnel{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&tunnelsv1alpha1.TunnelRoute{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				route := obj.(*tunnelsv1alpha1.TunnelRoute)
				return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: route.Spec.TunnelRef.Name}}}
			})).
		Named("clustertunnel").
		Complete(r)
}
