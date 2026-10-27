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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// dnsCommentPrefix marks DNS records the operator owns; it never edits records without it
// unless a route opts in with adoptExistingDnsRecord.
const dnsCommentPrefix = "managed-by=cloudflare-tunnel-operator"

func dnsComment(tunnel *tunnelsv1alpha1.ClusterTunnel) string {
	return dnsCommentPrefix + " tunnel=" + tunnel.Name
}

// TunnelRouteReconciler validates routes and manages their DNS records. The
// ClusterTunnel controller picks up accepted routes and pushes them into the tunnel config.
type TunnelRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	APIReader      client.Reader
	Cloudflare     cloudflare.Factory
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=tunnelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=tunnelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=tunnelroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=tunnels.latentlab.cc,resources=clustertunnels,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *TunnelRouteReconciler) resync() time.Duration {
	if r.ResyncInterval > 0 {
		return r.ResyncInterval
	}
	return DefaultResyncInterval
}

func (r *TunnelRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	route := &tunnelsv1alpha1.TunnelRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tunnel := &tunnelsv1alpha1.ClusterTunnel{}
	tunnelErr := r.Get(ctx, client.ObjectKey{Name: route.Spec.TunnelRef.Name}, tunnel)
	if tunnelErr != nil && !apierrors.IsNotFound(tunnelErr) {
		return ctrl.Result{}, tunnelErr
	}
	tunnelFound := tunnelErr == nil

	if !route.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, route, tunnel, tunnelFound)
	}

	if controllerutil.AddFinalizer(route, tunnelsv1alpha1.TunnelRouteFinalizer) {
		if err := r.Update(ctx, route); err != nil {
			return ctrl.Result{}, err
		}
	}
	base := route.DeepCopy()
	gen := route.Generation
	conds := &route.Status.Conditions

	var reconcileErr error
	result := ctrl.Result{RequeueAfter: r.resync()}

	accepted, reason, msg := false, "TunnelNotFound", fmt.Sprintf("ClusterTunnel %q does not exist", route.Spec.TunnelRef.Name)
	if tunnelFound {
		var err error
		accepted, reason, msg, err = r.validate(ctx, route, tunnel)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	if accepted {
		setCondition(conds, gen, tunnelsv1alpha1.ConditionAccepted, metav1.ConditionTrue, "Accepted", "route is in the tunnel config")
		route.Status.ServiceURL = route.ServiceURL()
	} else {
		setCondition(conds, gen, tunnelsv1alpha1.ConditionAccepted, metav1.ConditionFalse, reason, msg)
		route.Status.ServiceURL = ""
	}

	switch {
	case !tunnelFound:
		setCondition(conds, gen, tunnelsv1alpha1.ConditionDNSReady, metav1.ConditionFalse, "TunnelNotFound", msg)
	case !accepted:
		// Clean up DNS from when the route was previously accepted.
		if route.Status.DNSRecordID != "" {
			reconcileErr = r.withCloudflare(ctx, tunnel, func(cf cloudflare.API) error {
				return r.removeDNS(ctx, cf, route, tunnel)
			})
		}
		setCondition(conds, gen, tunnelsv1alpha1.ConditionDNSReady, metav1.ConditionFalse, "NotAccepted", "route is not accepted")
	case tunnel.Status.TunnelCNAME == "":
		setCondition(conds, gen, tunnelsv1alpha1.ConditionDNSReady, metav1.ConditionFalse, "TunnelNotReady", "waiting for the tunnel to be created")
	default:
		reconcileErr = r.withCloudflare(ctx, tunnel, func(cf cloudflare.API) error {
			return r.ensureDNS(ctx, cf, route, tunnel)
		})
		if reconcileErr != nil {
			var conflict *dnsConflictError
			if errors.As(reconcileErr, &conflict) {
				setCondition(conds, gen, tunnelsv1alpha1.ConditionDNSReady, metav1.ConditionFalse, "DNSRecordConflict", conflict.Error())
				reconcileErr = nil
			} else {
				setCondition(conds, gen, tunnelsv1alpha1.ConditionDNSReady, metav1.ConditionFalse, "DNSError", reconcileErr.Error())
			}
		} else {
			setCondition(conds, gen, tunnelsv1alpha1.ConditionDNSReady, metav1.ConditionTrue, "Ready",
				fmt.Sprintf("%s -> %s", route.Spec.Hostname, tunnel.Status.TunnelCNAME))
		}
	}

	if meta.IsStatusConditionTrue(*conds, tunnelsv1alpha1.ConditionAccepted) &&
		meta.IsStatusConditionTrue(*conds, tunnelsv1alpha1.ConditionDNSReady) {
		setReady(conds, gen, metav1.ConditionTrue, "Ready", "https://"+route.Spec.Hostname)
	} else {
		notReady := meta.FindStatusCondition(*conds, tunnelsv1alpha1.ConditionAccepted)
		if notReady.Status == metav1.ConditionTrue {
			notReady = meta.FindStatusCondition(*conds, tunnelsv1alpha1.ConditionDNSReady)
		}
		setReady(conds, gen, metav1.ConditionFalse, notReady.Reason, notReady.Message)
	}

	route.Status.ObservedGeneration = gen
	if err := r.Status().Patch(ctx, route, client.MergeFrom(base)); err != nil {
		log.Error(err, "patching TunnelRoute status")
		return ctrl.Result{}, errors.Join(reconcileErr, err)
	}
	return result, reconcileErr
}

// validate decides whether the route may be served. A false result with a nil
// error is a user-facing problem reported through the Accepted condition.
func (r *TunnelRouteReconciler) validate(ctx context.Context, route *tunnelsv1alpha1.TunnelRoute, tunnel *tunnelsv1alpha1.ClusterTunnel) (bool, string, string, error) {
	if sel := tunnel.Spec.NamespaceSelector; sel != nil {
		selector, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return false, "InvalidNamespaceSelector", err.Error(), nil
		}
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, client.ObjectKey{Name: route.Namespace}, ns); err != nil {
			return false, "", "", err
		}
		if !selector.Matches(labels.Set(ns.Labels)) {
			return false, "NamespaceNotAllowed",
				fmt.Sprintf("namespace %q is not allowed by ClusterTunnel %q", route.Namespace, tunnel.Name), nil
		}
	}

	svc := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: route.Namespace, Name: route.Spec.Service.Name}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "ServiceNotFound", fmt.Sprintf("service %q not found", route.Spec.Service.Name), nil
		}
		return false, "", "", err
	}
	hasPort := false
	for _, p := range svc.Spec.Ports {
		if p.Port == route.Spec.Service.Port {
			hasPort = true
			break
		}
	}
	if !hasPort {
		return false, "ServicePortNotFound",
			fmt.Sprintf("service %q has no port %d", route.Spec.Service.Name, route.Spec.Service.Port), nil
	}

	owner, err := r.hostnameOwner(ctx, route)
	if err != nil {
		return false, "", "", err
	}
	if owner != nil {
		return false, "HostnameConflict",
			fmt.Sprintf("hostname %q is already claimed by %s/%s", route.Spec.Hostname, owner.Namespace, owner.Name), nil
	}
	return true, "", "", nil
}

// hostnameOwner returns an older route that blocks this one, or nil. Routes on the
// same tunnel may share a hostname with different paths; across tunnels they can't,
// since there is only one DNS record per hostname.
func (r *TunnelRouteReconciler) hostnameOwner(ctx context.Context, route *tunnelsv1alpha1.TunnelRoute) (*tunnelsv1alpha1.TunnelRoute, error) {
	peers := &tunnelsv1alpha1.TunnelRouteList{}
	if err := r.List(ctx, peers, client.MatchingFields{indexRouteHostname: route.Spec.Hostname}); err != nil {
		return nil, err
	}
	for i := range peers.Items {
		peer := &peers.Items[i]
		if sameRoute(peer, route) || !peer.DeletionTimestamp.IsZero() || !olderThan(peer, route) {
			continue
		}
		if peer.Spec.TunnelRef.Name != route.Spec.TunnelRef.Name || peer.Spec.Path == route.Spec.Path {
			return peer, nil
		}
	}
	return nil, nil
}

func sameRoute(a, b *tunnelsv1alpha1.TunnelRoute) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name
}

// olderThan orders routes by creation time, breaking ties by namespace/name so
// exactly one route wins.
func olderThan(a, b *tunnelsv1alpha1.TunnelRoute) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
}

func (r *TunnelRouteReconciler) withCloudflare(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel, fn func(cloudflare.API) error) error {
	cf, err := cloudflareClients{Reader: r.APIReader, Factory: r.Cloudflare}.forTunnel(ctx, tunnel)
	if err != nil {
		return err
	}
	return fn(cf)
}

type dnsConflictError struct{ record *cloudflare.DNSRecord }

func (e *dnsConflictError) Error() string {
	return fmt.Sprintf("a %s record for %s already exists and is not managed by the operator; "+
		"set spec.adoptExistingDnsRecord to take it over", e.record.Type, e.record.Name)
}

func (r *TunnelRouteReconciler) ensureDNS(ctx context.Context, cf cloudflare.API, route *tunnelsv1alpha1.TunnelRoute, tunnel *tunnelsv1alpha1.ClusterTunnel) error {
	log := logf.FromContext(ctx).WithValues("hostname", route.Spec.Hostname)
	zoneID := tunnel.Spec.Cloudflare.ZoneID
	desired := cloudflare.DNSRecord{
		Type:    "CNAME",
		Name:    route.Spec.Hostname,
		Content: tunnel.Status.TunnelCNAME,
		Proxied: true,
		TTL:     1, // automatic
		Comment: dnsComment(tunnel),
	}

	recs, err := r.lookupDNS(ctx, cf, route, zoneID)
	if err != nil {
		return err
	}

	// Records we may write: anything the operator manages, plus foreign ones on opt-in.
	var candidates []cloudflare.DNSRecord
	for _, rec := range recs {
		if strings.HasPrefix(rec.Comment, dnsCommentPrefix) || route.Spec.AdoptExistingDNSRecord {
			candidates = append(candidates, rec)
		} else {
			return &dnsConflictError{record: &rec}
		}
	}

	if len(candidates) == 0 {
		rec, err := cf.CreateDNSRecord(ctx, zoneID, desired)
		if err != nil {
			return fmt.Errorf("creating DNS record: %w", err)
		}
		log.Info("created DNS record", "id", rec.ID, "target", desired.Content)
		route.Status.DNSRecordID = rec.ID
		return nil
	}

	// Keep the record we already track if it's there, otherwise the first one.
	primary := candidates[0]
	for _, rec := range candidates {
		if rec.ID == route.Status.DNSRecordID {
			primary = rec
		}
	}
	if primary.Type != desired.Type || primary.Content != desired.Content ||
		!primary.Proxied || primary.Comment != desired.Comment {
		if _, err := cf.UpdateDNSRecord(ctx, zoneID, primary.ID, desired); err != nil {
			return fmt.Errorf("updating DNS record: %w", err)
		}
		log.Info("updated DNS record", "id", primary.ID, "target", desired.Content,
			"previousType", primary.Type, "previousTarget", primary.Content, "previousComment", primary.Comment)
	}
	route.Status.DNSRecordID = primary.ID

	// A hostname can only have one CNAME; drop any extra copies this tunnel made.
	for _, rec := range candidates {
		if rec.ID != primary.ID && rec.Comment == dnsComment(tunnel) {
			if err := cf.DeleteDNSRecord(ctx, zoneID, rec.ID); err != nil && !cloudflare.IsNotFound(err) {
				return fmt.Errorf("deleting duplicate DNS record: %w", err)
			}
			log.Info("deleted duplicate DNS record", "id", rec.ID)
		}
	}
	return nil
}

// lookupDNS lists the hostname's records, and also fetches the record tracked in
// status by ID: the list endpoint can lag behind a create, and missing a record
// there would otherwise make us create a duplicate.
func (r *TunnelRouteReconciler) lookupDNS(ctx context.Context, cf cloudflare.API, route *tunnelsv1alpha1.TunnelRoute, zoneID string) ([]cloudflare.DNSRecord, error) {
	recs, err := cf.FindDNSRecords(ctx, zoneID, route.Spec.Hostname)
	if err != nil {
		return nil, fmt.Errorf("looking up DNS records: %w", err)
	}
	id := route.Status.DNSRecordID
	if id == "" {
		return recs, nil
	}
	for _, rec := range recs {
		if rec.ID == id {
			return recs, nil
		}
	}
	tracked, err := cf.GetDNSRecord(ctx, zoneID, id)
	switch {
	case cloudflare.IsNotFound(err):
		return recs, nil
	case err != nil:
		return nil, fmt.Errorf("fetching DNS record %s: %w", id, err)
	case strings.EqualFold(tracked.Name, route.Spec.Hostname):
		recs = append([]cloudflare.DNSRecord{*tracked}, recs...)
	}
	return recs, nil
}

// removeDNS deletes the hostname's records owned by this tunnel, unless another
// live route on the same tunnel still serves that hostname (under a different path).
func (r *TunnelRouteReconciler) removeDNS(ctx context.Context, cf cloudflare.API, route *tunnelsv1alpha1.TunnelRoute, tunnel *tunnelsv1alpha1.ClusterTunnel) error {
	log := logf.FromContext(ctx).WithValues("hostname", route.Spec.Hostname)

	peers := &tunnelsv1alpha1.TunnelRouteList{}
	if err := r.List(ctx, peers, client.MatchingFields{indexRouteHostname: route.Spec.Hostname}); err != nil {
		return err
	}
	for i := range peers.Items {
		peer := &peers.Items[i]
		if !sameRoute(peer, route) && peer.Spec.TunnelRef.Name == tunnel.Name && routeIsLive(peer) {
			log.Info("keeping DNS record shared with another route", "route", peer.Namespace+"/"+peer.Name)
			route.Status.DNSRecordID = ""
			return nil
		}
	}

	zoneID := tunnel.Spec.Cloudflare.ZoneID
	recs, err := r.lookupDNS(ctx, cf, route, zoneID)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Comment != dnsComment(tunnel) {
			log.Info("leaving DNS record not owned by this tunnel", "id", rec.ID, "comment", rec.Comment)
			continue
		}
		if err := cf.DeleteDNSRecord(ctx, zoneID, rec.ID); err != nil && !cloudflare.IsNotFound(err) {
			return fmt.Errorf("deleting DNS record: %w", err)
		}
		log.Info("deleted DNS record", "id", rec.ID)
	}
	route.Status.DNSRecordID = ""
	return nil
}

func (r *TunnelRouteReconciler) finalize(ctx context.Context, route *tunnelsv1alpha1.TunnelRoute, tunnel *tunnelsv1alpha1.ClusterTunnel, tunnelFound bool) error {
	if !controllerutil.ContainsFinalizer(route, tunnelsv1alpha1.TunnelRouteFinalizer) {
		return nil
	}
	if tunnelFound && route.Status.DNSRecordID != "" {
		err := r.withCloudflare(ctx, tunnel, func(cf cloudflare.API) error {
			return r.removeDNS(ctx, cf, route, tunnel)
		})
		if err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(route, tunnelsv1alpha1.TunnelRouteFinalizer)
	if err := r.Update(ctx, route); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TunnelRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	routesFor := func(field, value string) []reconcile.Request {
		routes := &tunnelsv1alpha1.TunnelRouteList{}
		if err := mgr.GetClient().List(context.Background(), routes, client.MatchingFields{field: value}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(routes.Items))
		for _, route := range routes.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&route)})
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&tunnelsv1alpha1.TunnelRoute{}).
		// Tunnel changes (new CNAME, namespace selector) affect all of its routes.
		Watches(&tunnelsv1alpha1.ClusterTunnel{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return routesFor(indexRouteTunnel, obj.GetName())
			})).
		// A route appearing or going away can resolve another route's hostname conflict.
		Watches(&tunnelsv1alpha1.TunnelRoute{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return routesFor(indexRouteHostname, obj.(*tunnelsv1alpha1.TunnelRoute).Spec.Hostname)
			})).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				return routesFor(indexRouteService, obj.GetNamespace()+"/"+obj.GetName())
			})).
		Named("tunnelroute").
		Complete(r)
}
