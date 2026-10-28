# Testing strategy

Status as of 2026-10-28 (v0.1.0 plus unreleased changes: tunnel rename, the Ginkgo port of the controller specs, and the move to the cloudflare-go SDK; all live-tested on 2026-10-28).

The operator has three automated layers and one manual one. The automated layers are fast
and catch logic regressions; they do **not** prove the operator works against a real API
server or the real Cloudflare API. That is currently covered only by a manual live run on
the home lab cluster (last run 2026-10-27).

| Layer | What runs | Where | Time | Talks to |
|---|---|---|---|---|
| 1. Cloudflare client tests | `internal/cloudflare/client_test.go` (plain `go test`) | `make test`, CI on every push | <1s | `httptest` server |
| 2. Controller specs | `internal/controller/*_test.go` (Ginkgo + Gomega) | `make test`, CI on every push | ~1s | controller-runtime fake client + `cloudflare.Fake` |
| 3. Scaffold e2e | `test/e2e` (kubebuilder default) | `make test-e2e`, CI on every push | minutes | Kind cluster, no Cloudflare |
| 4. Live run | Manual steps below | Home lab cluster | ~30 min | Real cluster + real Cloudflare |

Static checks run alongside: `make lint` (golangci-lint, config in `.golangci.yml`) and
`go vet`. The release workflow runs `make test` and `make lint` again before publishing.

## 1. Cloudflare client tests

`internal/cloudflare.Client` implements the `API` interface with the official
[cloudflare-go](https://github.com/cloudflare/cloudflare-go) SDK (v7.12.0). The tests point
it at an `httptest` stub server that records each request and returns canned Cloudflare
response envelopes. Like the real API, the stub answers requests for page 2 or later with an
empty page: the SDK's auto-pager only stops on an empty page, so every list call makes one
extra request.

- `TestClientRequests` is one table covering **all 13 methods**: method, path, query
  (e.g. `name.exact=` for DNS lookups, `is_deleted=false` for tunnels), the bearer token,
  the exact JSON body, and how the response maps back to our types. It checks that unset
  fields are omitted, so the catch-all ingress rule goes out as just
  `{"service": "http_status:404"}`, and that `FindDNSRecords` keeps only CNAME/A/AAAA
  records whose name matches exactly, so a change in the API's filter can never widen a
  delete.
- `TestClientNotFound`: a 404 satisfies `IsNotFound`, keeps Cloudflare's message, and is not
  retried.
- `TestClientRetriesRateLimits`: a 429 followed by a 200 succeeds after the SDK's backoff.
- `TestClientRejectsNonCNAMEWrites`: the client refuses to write anything but a CNAME,
  without sending a request.
- `TestIsNotFoundMatchesFakeErrors`: the Fake's errors behave like the SDK's for
  `IsNotFound`.

Coverage: 45% of `internal/cloudflare`. The number understates it, because `fake.go` is in
the same package and is only exercised from the controller specs.

These tests prove the requests match what we believe the API expects. They don't prove the
API agrees; the live run does that.

## 2. Controller specs

These are the main safety net, written as Ginkgo specs with Gomega assertions. The client
tests above stay plain `go test`: they check one function at a time, which is idiomatic
table-style Go, while the controller specs describe object lifecycles.

| File | Contents |
|---|---|
| `suite_test.go` | The single `TestControllers` entry point that runs the Ginkgo suite |
| `harness_test.go` | `newHarness`, object builders and helpers shared by both spec files |
| `clustertunnel_controller_test.go` | `Describe("ClusterTunnel controller")` |
| `tunnelroute_controller_test.go` | `Describe("TunnelRoute controller")` |

Each spec builds a fresh harness, in its `BeforeEach` or in the `It` itself, so specs
never share state:

- a controller-runtime **fake client** with the scheme, status subresources for both CRDs,
  and the same three field indexes the manager registers (`spec.tunnelRef.name`,
  `spec.hostname`, `spec.service`);
- seed objects: the operator and app namespaces, the API token Secret, and a `kong-proxy`
  Service;
- a `cloudflare.Fake`: an in-memory implementation of the `cloudflare.API` interface that
  stores tunnels, configs and DNS records, counts config pushes and renames, and can hide
  records from list calls to simulate the list endpoint lagging behind creates.

Specs call `Reconcile` directly and in a chosen order, then assert on Kubernetes objects and
on the fake's state. There is no manager, no cache, and no watches. Helpers call
`GinkgoHelper()`, so a failure is reported at the spec's line, not inside the helper.
`BeforeEach` setup is kept to one level per `Context`.

Run a subset with Ginkgo's focus flag, which matches against the full spec text:

```sh
go test ./internal/controller/ -v -args -ginkgo.focus="same hostname"
```

**ClusterTunnel controller** (14 specs)

| Context | Specs |
|---|---|
| for a new tunnel | Creates a managed tunnel (ID, name, CNAME target in status); stores the token in an owned Secret; runs 2 pinned cloudflared replicas with an owner ref and a PDB; pushes a catch-all-only config; reports `Progressing` until replicas are ready; idempotent (no second tunnel, no unchanged config push); deleting the ClusterTunnel deletes the tunnel in Cloudflare and releases the finalizer |
| renaming | Renames the same tunnel ID in place; reverts a dashboard rename and skips the call when the name already matches; clearing `tunnelName` returns to `metadata.name` |
| adopting an existing tunnel | Uses it without marking it managed; keeps its own name unless `tunnelName` is set; deleting the ClusterTunnel leaves it in Cloudflare |
| when the API token Secret is missing | `CredentialsUnavailable` condition and a requeue, not a hard error |

**TunnelRoute controller** (15 specs)

| Context | Specs |
|---|---|
| for a valid route | Ready with the in-cluster `serviceUrl`; proxied CNAME to the tunnel; added to the tunnel config and counted in `status.routes`; deleting it removes the DNS record, the finalizer and the config rule |
| when two routes claim the same hostname | The older is accepted and the newer gets `HostnameConflict`; the newer takes over once the older is deleted |
| when routes share a hostname with different paths | Both accepted; the path rule sorts before the bare hostname; deleting one keeps the shared DNS record |
| validation | `namespaceSelector` rejects the route with no DNS created; `ServiceNotFound` |
| when the hostname already has a DNS record the operator didn't create | `DNSRecordConflict` with the record untouched; `adoptExistingDnsRecord` repoints and tags it |
| DNS edge cases | A record missing from the list call but tracked in status is found by ID, so no duplicate, and is still deleted; duplicate managed records collapse to one and are all removed on delete, leaving unrelated records |

Coverage: 77% of `internal/controller`.

What the fake client does **not** give us, and so these tests can't catch:

- **No API server behaviour.** CRD schema validation, defaulting (`replicas: 2`,
  `scheme: http`, `status.routes: 0`), and print columns are never exercised. Objects that
  real validation would reject are accepted.
- **No generated metadata.** UIDs and creation timestamps aren't set; tests set
  `creationTimestamp` by hand where ordering matters. This once hid a bug: code that
  compared UIDs passed nothing, so peers compared equal. It now compares namespace/name.
- **No watches.** `SetupWithManager` and its mapping functions (tunnel → routes,
  route → same-hostname routes, Service → routes) never run, so "a Service appearing
  re-reconciles its route" is untested here.
- **No cache staleness or concurrency.** Each `Reconcile` reads the latest object. The
  double-finalize conflict found in the live run (fixed by patching finalizers instead of
  `Update`) can't be reproduced in this layer.
- **No garbage collection.** Owner references are asserted, but nothing deletes owned
  objects when the ClusterTunnel goes away.

## 3. Scaffold e2e (Kind)

This is kubebuilder's default suite, unchanged. CI creates a Kind cluster, builds the image,
runs `make deploy`, then checks two things: the controller-manager pod reaches `Running`,
and its metrics endpoint serves metrics over HTTPS with a ServiceAccount token.

It proves the image, RBAC and kustomize manifests produce a manager that starts under the
`restricted` Pod Security level. It creates **no** ClusterTunnels or TunnelRoutes and never
calls Cloudflare.

## 4. Live run on the home lab cluster

The only end-to-end check of real behaviour. First run 2026-10-27, operator running locally
(`go run ./cmd`) against the cluster, a throwaway `operator-test` tunnel and
`tunnel-operator-*.latentlab.cc` hostnames, with a `traefik/whoami` backend.

| Scenario | Checked by | Result |
|---|---|---|
| CRD defaults and validation | `kubectl apply --dry-run=server` with good and bad objects | Pass |
| Tunnel creation | Tunnel ID in status; 2 cloudflared pods on different nodes; edge registered 4 connections; config `version=1` | Pass |
| Route → traffic | Route Ready in ~2s; config pushed live; `curl` through the edge reached whoami (`Cf-Ray` header, HTTP 200) | Pass |
| Hostname conflict and handover | Second route rejected; took over after the first was deleted | Pass |
| Service appearing later | Route went Ready when the Service was created (watch path) | Pass |
| DNS adoption | Refused without opt-in; with opt-in, same record ID updated in place; traffic through the new tunnel | Pass |
| Route and tunnel deletion | DNS deleted, config back to catch-all, cloudflared/Secret/PDB removed, tunnel deleted in Cloudflare | Pass, after two fixes below |

Found and fixed during the run:

- Removing finalizers with `Update` conflicted when the object changed mid-deletion, so
  cleanup ran twice (error logs, a second `DeleteTunnel`). Now a merge patch.
- The `ROUTES` print column was blank at 0. Now defaulted in the CRD.
- DNS lookups trusted the API's name filter and took the first record. Hardened to exact
  name matching, duplicate collapse and lookup by tracked ID.

Learned: Cloudflare's authoritative DNS can keep answering for a deleted proxied record for
**up to ~30 minutes**. Judge DNS cleanup by the dashboard or operator logs, never by `dig`.

Also verified after the v0.1.0 release: the published image runs in-cluster, holds its
leader lease, created the shared `home` tunnel, and served `grpc-demo.latentlab.cc` end to
end (web and gRPC API through Kong).

**2026-10-28: SDK client and rename.** With the in-cluster operator scaled to 0, a local build
on the cloudflare-go SDK reconciled the live `home` tunnel and grpc-demo route, then a throwaway
`sdk-test` tunnel and `tunnel-operator-sdk.latentlab.cc` route:

| Scenario | Checked by | Result |
|---|---|---|
| Existing tunnel and route | `home` and `greeter` stayed Ready; config re-pushed through the SDK; grpc-demo HTTP 200 | Pass. The connectors stayed on config `version=2`: Cloudflare only versions changed content, so the SDK sent a config identical to the old client's |
| New tunnel, DNS and traffic | Tunnel created, 4 edge connections per pod, config received, CNAME created, `curl` through the edge reached whoami (HTTP 200) | Pass |
| Rename | `tunnelName` set: same tunnel ID, name read back from Cloudflare, 0 pod restarts and 0 reconnects, traffic still 200; cleared: renamed back; exactly 2 rename calls | Pass |
| Cleanup | DNS record and tunnel deleted | Pass |

Notes from this run: the cluster still had the v0.1.0 CRD, which silently dropped the new
`status.tunnelName` until `make install` updated it, so **CRDs must be upgraded with the
operator**. The finalizer ran twice on tunnel deletion again (a stale cache read), now without
errors since a second delete succeeds. And the first local start failed to sync its caches
during a Tailscale blip to the API server, which is unrelated to the operator.

Not yet live-tested: path-based routes, `namespaceSelector`,
credential rotation, and Cloudflare API errors or rate limits.

### Running it again

The live run uses real Cloudflare resources, so use throwaway names.

1. Make sure the in-cluster operator won't fight a local one. Either test with the deployed
   operator, or scale it to 0 first:
   `kubectl -n cloudflare-tunnel-operator-system scale deploy/cloudflare-tunnel-operator-controller-manager --replicas=0`.
2. Run the operator locally: `make run` (or build and run `./cmd`), or deploy a release.
3. Create a test `ClusterTunnel` with its own name, a namespace with a small HTTP backend,
   and `TunnelRoute`s for `tunnel-operator-*.latentlab.cc` hostnames.
4. Check conditions with `kubectl get ctun,troute -A`, operator logs for the DNS and tunnel
   actions, and traffic with `curl --resolve <host>:443:<cloudflare-ip>` (get the IP from
   the zone's authoritative nameserver) so local DNS caching doesn't mislead you.
5. Delete routes, then the tunnel, and confirm in the operator log that each DNS record and
   the tunnel were deleted. Scale the in-cluster operator back up.

## Gaps and what I'd add, in priority order

1. **envtest suite for the controllers.** Run the reconcilers under a real manager against
   kubebuilder's envtest API server (already downloaded by `make test`), still with
   `cloudflare.Fake`, as more Ginkgo specs using Gomega's `Eventually` for the
   asynchronous reconciles. This covers schema validation and defaults, real status subresource
   and finalizer semantics, UIDs and timestamps, and the watch mappings in
   `SetupWithManager`. It's the biggest gap, and the double-finalize bug would have shown
   up here.
2. **A scripted live smoke test** (`make smoke`) that automates the live-run steps against a
   throwaway tunnel and hostname, with cleanup in a `trap`. Run it by hand before each
   release; it needs the real API token, so not in public-repo CI.
3. **Extend the e2e suite** to create a ClusterTunnel and TunnelRoute in Kind, pointing
   `--cloudflare-api-url` at a small fake Cloudflare server running in the cluster. That
   exercises the released image's whole reconcile path in CI without real credentials.
4. **Failure-path tests in the controller specs**: a Cloudflare 5xx surfacing in conditions, the token Secret changing,
   and a tunnel deleted out from under the operator (the `IsNotFound` recreate branch).
