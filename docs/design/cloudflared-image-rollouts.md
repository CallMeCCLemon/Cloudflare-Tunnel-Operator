# Design: automated cloudflared image rollouts

Status: proposed · 2026-10-27

## Problem

Every project's cloudflared runs `cloudflare/cloudflared:latest` with `imagePullPolicy: Always`.
There is no custom image; we use upstream directly. The version a pod runs is whatever `latest`
pointed to when the pod last restarted, so on 2026-10-27 the cluster was running a mix of
**2026.9.1, 2026.10.0 and 2026.10.1**, sometimes within a single Deployment, while upstream was
on 2026.10.4. Upgrades only happen by accident (node reboot, eviction), and when one is needed
on purpose, someone has to notice and restart every project's Deployment by hand.

The operator today pins `2026.10.4` as a compiled-in default (`DefaultCloudflaredImage`), with
`spec.cloudflared.image` as a free-form override. That stops drift but means an upgrade
requires an operator release, and nothing detects, verifies, or rolls back an upgrade.

## Goals

1. **Deterministic**: every replica runs exactly the same, known build. No `latest`, no tag mutation.
2. **Pinnable**: the `ClusterTunnel` can freeze cloudflared at a specific version.
3. **Automatic**: new upstream releases are picked up without anyone editing manifests.
4. **Safe**: a rollout never drops below one live connector, is verified end to end through
   Cloudflare's edge, and rolls back by itself if the new version misbehaves.
5. **Visible**: `kubectl get ctun` shows running vs. available versions and the rollout state.

Not covered: the operator's own image (shipped through the Helm chart's version) and
application images in each project.

## Decision: who owns the version

| Option | How | Pros | Cons |
|---|---|---|---|
| A. Git pin + Renovate | Renovate PRs bump `spec.cloudflared.version` in the manifest; merge, then `kubectl apply` | Audit trail in git | We have no GitOps controller, so a person still has to merge and apply. That's exactly the manual step we want gone. |
| B. Operator writes `spec` | Operator patches its own spec to the new version | Version visible in spec | Fights `kubectl apply`/Helm, which revert it on the next apply |
| **C. Pin in spec, auto target in status** | `spec.cloudflared.version` is an optional hard pin; with no pin, the operator picks the target itself and records it in `status` | Fully automatic; explicit pin always wins; no fight with apply | The auto-chosen version lives in status, not git |

**Choose C.** It keeps "pin the version in the CRD" as the control: set `version` to freeze or
to force a specific upgrade, clear it to go back to automatic. Whatever the source, the
Deployment always gets an **immutable digest** (`cloudflare/cloudflared@sha256:…`), so
"pinned" holds at the pod level in both modes. Option A can be added later if we adopt
Flux or Argo CD; the spec field it would bump is the same one.

## API changes

`spec.cloudflared.image` is replaced (v1alpha1, no users yet):

```yaml
spec:
  cloudflared:
    namespace: cloudflare-tunnels
    replicas: 2
    repository: docker.io/cloudflare/cloudflared   # default
    version: ""            # e.g. "2026.10.4" pins it. Empty = follow updatePolicy.
    updatePolicy:
      minReleaseAge: 72h   # don't take releases younger than this (bad releases get superseded fast)
      checkInterval: 6h    # how often to look for new releases
      soakTime: 10m        # how long a new version must stay healthy before it counts as good
      rollback: true       # auto-revert a failed automatic rollout
      healthCheckUrl: ""   # optional: https URL served through this tunnel, checked during verify
      maintenanceWindow:   # optional: only *start* automatic rollouts inside this window
        days: [Tue, Wed, Thu]
        start: "10:00"
        duration: 4h
        timeZone: Asia/Seoul
```

New status block:

```yaml
status:
  cloudflared:
    currentVersion: 2026.10.1
    currentDigest: sha256:a15a3b…
    targetVersion: 2026.10.4
    targetDigest: sha256:…
    targetSource: Auto          # Pinned | Auto
    latestAvailable: 2026.10.4  # newest upstream release, whatever its age
    lastGoodVersion: 2026.10.1  # rollback target
    failedVersions: [2026.10.3] # automatic mode won't retry these
    phase: Verifying            # Idle | Waiting | RollingOut | Verifying | RollingBack
    lastChecked: 2026-10-27T06:00:00Z
  conditions:
    - type: UpToDate            # current == newest eligible release
    - type: UpdateAvailable     # a newer release exists (including a pinned tunnel falling behind)
    - type: Updating
    - type: UpdateFailed        # last rollout failed; message says why
```

New print columns: `VERSION` (current) and `TARGET`.

## Components

### 1. Release source

A `releases.Source` interface, polled every `checkInterval` and shared by all tunnels (one cache):

- **Version list and age**: GitHub Releases for `cloudflare/cloudflared`, skipping drafts and
  pre-releases, with `published_at` used for `minReleaseAge`. Docker Hub tags have no reliable
  publish date for this, and GitHub also gives us release notes to link in Events.
- **Digest**: resolve `repository:version` to its multi-arch index digest with
  `go-containerregistry` (a `HEAD` on the manifest). The node arches (amd64 now, arm64 possible
  later) are covered by the index digest. Manifest HEADs don't count against Docker Hub's pull
  rate limit.
- **Ordering**: cloudflared uses calendar versions (`YYYY.M.P`). Parse them as three integers
  and compare numerically, never as strings.

The source is an interface so tests use a fake, and a private mirror can be added later
(`repository` pointing at the local registry plus a mirror job).

### 2. Target selection (each reconcile)

```
if spec.version != "":          target = spec.version            (source: Pinned)
else:                            target = newest release where
                                   age >= minReleaseAge
                                   and version not in failedVersions
                                   and version >= currentVersion   (never auto-downgrade)
                                                                   (source: Auto)
targetDigest = resolve(repository, target)   // cached per version; digests don't change
```

If resolution fails (registry down, typo in a pin), the Deployment is left alone and
`UpdateFailed` is set with the reason. A bad pin can never produce a broken pod spec.

On first adoption with no history, `currentVersion` is read from the running Deployment, and
from the connector versions Cloudflare reports when the Deployment uses `latest`.

### 3. Rollout state machine

```
         target != current
 Idle ───────────────────────► Waiting ──(window open, or Pinned)──► RollingOut
  ▲                                                                     │ patch Deployment image@digest
  │                                                                     ▼
  │◄──── soak passed: lastGood = target ───────────────────────── Verifying
  │                                                                     │ failure
  │                                                                     ▼
  └──── restore lastGood digest, add target to failedVersions ─── RollingBack
```

- **RollingOut**: the Deployment uses `strategy: RollingUpdate, maxSurge: 1, maxUnavailable: 0`
  and `minReadySeconds: 30`, so one new pod has to pass `/ready` (an active edge connection)
  before an old one is removed. With 2 replicas plus the PDB, at least 2 connectors are up at
  all times. A version that never becomes ready simply stalls with the old pods still serving.
- **Verifying** passes once all of these hold continuously for `soakTime`:
  1. the Deployment is `Available`, with `updatedReplicas == readyReplicas == replicas`;
  2. Cloudflare's tunnel connections endpoint (`GET …/cfd_tunnel/{id}/connections`) reports
     every connector on `targetVersion`. This is the end-to-end proof that the edge sees the
     new build, not only Kubernetes;
  3. the tunnel's `status` is `healthy`;
  4. `healthCheckUrl`, if set, returns 2xx/3xx through the edge.
- **Failure** means `progressDeadlineSeconds` (300s) is exceeded, or any check above fails for
  more than 3 minutes during verify, or connector restarts go above a threshold.
  - **Auto** target: go to RollingBack (`lastGoodVersion` digest), add the version to
    `failedVersions`, set `UpdateFailed` and emit a Warning Event. A newer release clears the
    way, and so does deleting the entry from status.
  - **Pinned** target: don't roll back (that would fight the user's explicit choice). Report
    `UpdateFailed`. `maxUnavailable: 0` keeps the old pods serving if the new ones never get
    ready.
- The **maintenance window** only gates *starting* automatic rollouts. Verification and
  rollback always run. Pinned changes apply right away, since editing the pin is the manual
  "do it now".

### 4. Visibility

- Conditions and print columns as above.
- Events: `UpdateAvailable`, `RolloutStarted`, `RolloutSucceeded`, `RolloutFailed`,
  `RolledBack`, each with old → new version and a release notes link.
- Prometheus: `cto_cloudflared_info{tunnel,version}`,
  `cto_cloudflared_rollouts_total{tunnel,result}`, `cto_cloudflared_versions_behind{tunnel}`.
- An `Outdated` condition when the running version is more than 90 days behind the newest
  release, as an early warning before Cloudflare stops supporting old connectors. This is the
  kind of upgrade that recently had to happen by hand.

## Rollout of this change

1. **Now (interim, independent of the operator):** pin the four existing per-project
   Deployments (cupr, immich, modernmtg, riftbound) to `cloudflare/cloudflared:2026.10.4` to
   stop the drift until they move to the shared tunnel. Three cloudflared pods are also stuck
   `Terminating` on `hp-gpu-workstation`, which is `NotReady`.
2. **Phase 1, digest pinning:** replace `image` with `repository` + `version`, resolve to a
   digest, add the `status.cloudflared` current/target fields and the RollingUpdate settings.
   This alone gives deterministic, pinnable rollouts.
3. **Phase 2, auto updates:** release source, target selection with `minReleaseAge`, and the
   `UpdateAvailable`/`UpToDate` conditions.
4. **Phase 3, verify and rollback:** the connections API check, soak, rollback,
   `failedVersions`, Events.
5. **Phase 4, polish:** maintenance window, `healthCheckUrl`, metrics, `Outdated`.

## Testing

- Unit: version parsing and ordering, target selection (pin wins, `minReleaseAge`,
  `failedVersions`, no downgrade), and each state-machine transition with fake release and
  Cloudflare sources and a fake clock.
- Live on the home lab test tunnel:
  1. Pin `2026.10.1`, then clear the pin with `minReleaseAge: 0`. Expect an automatic upgrade
     to the newest release, connectors reporting the new version, and `phase` back to `Idle`.
  2. Pin a version whose image starts but never becomes ready (for example override the args
     in a test build) and confirm the old pods keep serving and `UpdateFailed` is set.
  3. In automatic mode, inject a failing `healthCheckUrl` and confirm the rollback and that
     the version lands in `failedVersions`.

## Open questions

- Is a 72h `minReleaseAge` right? cloudflared ships several releases a month, so 72h usually
  means taking the release before the latest.
- Do we want notifications outside the cluster (Discord or Slack webhook) for `RolledBack`
  and `Outdated`, or are Events and metrics enough?
- If we later run more than one `ClusterTunnel`, should rollouts be staggered (one tunnel
  verifies before the next starts)? The `failedVersions` list could be shared through a small
  cluster-scoped `CloudflaredRelease` object.
