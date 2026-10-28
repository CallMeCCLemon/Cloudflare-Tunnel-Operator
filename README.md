# Cloudflare-Tunnel-Operator

A Kubernetes operator that runs **one shared Cloudflare tunnel per cluster** and lets each
project publish hostnames with a small `TunnelRoute`, instead of every project shipping its
own cloudflared Deployment, tunnel token, and dashboard-configured routes.

```
grpc.latentlab.cc ─┐
immich.latentlab.cc ┼─► Cloudflare edge ─► ClusterTunnel "home" (2× cloudflared, ns cloudflare-tunnels)
mtg.latentlab.cc ──┘                          ├─► kong-proxy.grpc-demo.svc:80
                                              ├─► immich-server.immich.svc:2283
                                              └─► mtg-web.mtg.svc:80
```

## Resources

**`ClusterTunnel`** (cluster-scoped) — the tunnel and its connectors. The operator:

- creates a remotely-managed tunnel in Cloudflare (or adopts one via `spec.existingTunnelId`),
- stores the run token in `cloudflared-<name>-token` and runs `cloudflared-<name>`
  (2 replicas, spread across nodes, PodDisruptionBudget `minAvailable: 1`),
- merges every accepted `TunnelRoute` into the tunnel's ingress config, ending in a
  catch-all `http_status:404`, and re-pushes it every resync (default 10m), so edits made in
  the dashboard are overwritten,
- on deletion, removes the tunnel from Cloudflare **only if the operator created it**.

**`TunnelRoute`** (namespaced) — one hostname (and optional path regex) to a Service in the
route's own namespace. The operator:

- accepts it only if the Service and port exist, the namespace matches the tunnel's
  `namespaceSelector`, and no older route claims the same hostname,
- creates a proxied CNAME `hostname → <tunnel-id>.cfargotunnel.com`, tagged with a
  `managed-by=cloudflare-tunnel-operator` comment,
- won't touch a DNS record it didn't create unless `spec.adoptExistingDnsRecord: true`,
- removes the DNS record on deletion (unless another route on the tunnel shares the hostname).

```console
$ kubectl get ctun
NAME   TUNNEL ID      HEALTH    ROUTES   READY
home   6ff42ae2-...   healthy   3        True

$ kubectl get troute -A
NAMESPACE   NAME   HOSTNAME            TUNNEL   SERVICE                                         READY
grpc-demo   web    grpc.latentlab.cc   home     http://kong-proxy.grpc-demo.svc.cluster.local:80   True
```

Samples: [config/samples](config/samples).

## Cloudflare API token

Create an API token with:

- **Account → Cloudflare Tunnel → Edit**
- **Zone → DNS → Edit** (for the zone in `spec.cloudflare.zoneId`)

```sh
kubectl create namespace cloudflare-tunnels
kubectl -n cloudflare-tunnels create secret generic cloudflare-api --from-literal=apiToken=<token>
```

## Releasing

Pushing a semver tag runs [the release workflow](.github/workflows/release.yml): tests and lint,
then a multi-arch (`linux/amd64`, `linux/arm64`) image is pushed to
`ghcr.io/callmecclemon/cloudflare-tunnel-operator`, and a GitHub Release is created with an
`install.yaml` pinned to that image's digest.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Tags like `v0.2.0-rc.1` publish a pre-release and don't move `latest`.

## Deploying

Deploys are manual. Apply a release:

```sh
kubectl apply -f https://github.com/CallMeCCLemon/Cloudflare-Tunnel-Operator/releases/download/v0.1.0/install.yaml
kubectl apply -f config/samples/tunnels_v1alpha1_clustertunnel.yaml
```

Or deploy from a checkout: `make deploy IMG=ghcr.io/callmecclemon/cloudflare-tunnel-operator:0.1.0`.

Remove with `make undeploy`. Delete `TunnelRoute`s and `ClusterTunnel`s first so their
finalizers can clean up Cloudflare.

Manager flags: `--cloudflare-api-url` (default `https://api.cloudflare.com/client/v4`) and
`--resync-interval` (default `10m`).

## Migrating an existing project

1. Add a `TunnelRoute` for the project's hostname with `adoptExistingDnsRecord: true`.
   The CNAME is repointed from the project's old tunnel to the shared one.
2. Check the route is `Ready` and the site works.
3. Delete the project's `cloudflared-deployment`, its tunnel-creds Secret, and the old tunnel
   in the Cloudflare dashboard.
4. Drop `adoptExistingDnsRecord` and remove the cloudflared template from the project.

## Development

```sh
make manifests generate   # regenerate CRDs, RBAC, deepcopy
make test                 # unit + controller tests (fake Cloudflare API)
make run                  # run against the current kube context
```

`internal/cloudflare` wraps the official [cloudflare-go](https://github.com/cloudflare/cloudflare-go)
SDK (v7) behind a small `API` interface covering the tunnel and DNS calls the operator makes. The SDK
retries 429 and 5xx responses with backoff. `cloudflare.Fake` implements the same interface in memory
for the controller specs.

## License

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
