package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunnelsv1alpha1 "github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/api/v1alpha1"
	"github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/internal/cloudflare"
)

// Field indexes on TunnelRoute, registered once in SetupIndexes.
const (
	indexRouteTunnel   = "spec.tunnelRef.name"
	indexRouteHostname = "spec.hostname"
	indexRouteService  = "spec.service"
)

// SetupIndexes registers the TunnelRoute field indexes both controllers rely on.
func SetupIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	indexes := map[string]client.IndexerFunc{
		indexRouteTunnel:   routeTunnelIndex,
		indexRouteHostname: routeHostnameIndex,
		indexRouteService:  routeServiceIndex,
	}
	for field, fn := range indexes {
		if err := indexer.IndexField(ctx, &tunnelsv1alpha1.TunnelRoute{}, field, fn); err != nil {
			return fmt.Errorf("indexing %s: %w", field, err)
		}
	}
	return nil
}

func routeTunnelIndex(obj client.Object) []string {
	return []string{obj.(*tunnelsv1alpha1.TunnelRoute).Spec.TunnelRef.Name}
}

func routeHostnameIndex(obj client.Object) []string {
	return []string{obj.(*tunnelsv1alpha1.TunnelRoute).Spec.Hostname}
}

func routeServiceIndex(obj client.Object) []string {
	route := obj.(*tunnelsv1alpha1.TunnelRoute)
	return []string{route.Namespace + "/" + route.Spec.Service.Name}
}

// cloudflareClients builds Cloudflare API clients from a ClusterTunnel's credentials Secret.
type cloudflareClients struct {
	// Reader is used for the credentials Secret; an uncached reader avoids
	// caching every Secret in the cluster.
	Reader  client.Reader
	Factory cloudflare.Factory
}

func (c cloudflareClients) forTunnel(ctx context.Context, tunnel *tunnelsv1alpha1.ClusterTunnel) (cloudflare.API, error) {
	ref := tunnel.Spec.Cloudflare.APITokenSecretRef
	key := ref.Key
	if key == "" {
		key = "apiToken"
	}
	secret := &corev1.Secret{}
	if err := c.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		return nil, fmt.Errorf("reading API token secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	token := string(secret.Data[key])
	if token == "" {
		return nil, fmt.Errorf("API token secret %s/%s has no %q key", ref.Namespace, ref.Name, key)
	}
	return c.Factory(token), nil
}
