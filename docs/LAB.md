# The Spinnaker lab

`lab/setup.sh` stands up a complete Spinnaker on an existing Kubernetes cluster,
for developing `sc` and running its end-to-end suite.

```bash
lab/setup.sh
lab/setup.sh --delete
```

| Variable | Default | Meaning |
|:--|:--|:--|
| `SPINNAKER_VERSION` | `2026.3.0` | release tag to pin every image to |
| `SPINNAKER_NAMESPACE` | `spinnaker` | target namespace |
| `SPINNAKER_HOST` | `spinnaker.<ingress-ip>.nip.io` | hostname for Deck and Gate |
| `SPINNAKER_INGRESS_CLASS` | `traefik` | `ingressClassName` on the Ingress |
| `SPINNAKER_WORKDIR` | `~/spinnaker-lab` | rendered working copy |
| `SPINNAKER_SRCDIR` | `~/spinnaker-src` | monorepo clone |

Requirements: a cluster with roughly **16 GB of memory and 6 cores** free (eleven
JVM services, ten requesting 1 GiB each, plus MySQL and Redis), `kubectl` with
kustomize, and `git`.

> **Private clusters only.** The upstream base grants Spinnaker `cluster-admin`,
> enables no real authentication, and sets a default password. The lab overlay
> additionally exposes actuator endpoints that print resolved configuration
> values, which include credentials.

## What it installs

The upstream **kustomize base** from the Spinnaker monorepo
(`spinnaker/spinnaker//spinnaker-kustomize`). That is the supported install path:
Halyard was fully deprecated in 2026.1.0 in favor of "spinnaker kustomize", and
the standalone `spinnaker/spinnaker-kustomize` repo has been archived into the
monorepo.

Deployed: clouddriver, deck, echo, fiat, front50, gate, igor, kayenta, keel, orca,
rosco, plus MySQL and Redis. Images come from GHCR, which became the default
registry in 2026.1.0 (GAR is being retired for images).

Two things the base gives you for free that matter here:

- **A working Kubernetes account.** `base/clouddriver/files/clouddriver.yml`
  configures an in-cluster account named `managing` using the pod's service
  account, with a ClusterRoleBinding to `cluster-admin`. Deployments work
  immediately.
- **A single-ingress layout.** Gate is mounted under the servlet context path
  `/api/v1` and Deck at `/`, so one host serves both without CORS. This is why
  `sc login --gate` must include `/api/v1`.

## What the script changes

1. **Pins the image tags.** The base ships `newTag: main-latest`, a moving target.
2. **Sets the hostname in three places**, which must agree or logins redirect to
   the wrong origin and Deck calls the wrong API base:
   - `base/spinnaker.yml` → `services.deck.baseUrl` (Gate validates login
     redirects against it)
   - `base/deck/files/settings.js` → `gateHost`
   - `ingress.yaml` → the rule host, plus `ingressClassName`
3. **Applies `lab/overlays/spinnaker-local.yml`** — the actuator configuration.
4. **Appends `lab/overlays/orca-local.yml`** — two orca fixes.

## lab/overlays/spinnaker-local.yml — the operator plane

Mounted into every JVM service (Spinnaker services load `spinnaker.yml` and
`<service>.yml` plus their `-local` profile variants).

Without it, `sc svc env`, `loggers`, `metrics`, `threads`, `configprops`,
`mappings` and `beans` all return 404, because:

- kork's `SpringBoot1CompatibilityApplicationListener` sets
  `management.endpoints.web.base-path=/` so `/health` stays where it has always
  been — actuator endpoints are at the service **root**, not `/actuator/*`;
- Spring Boot exposes only `/health` by default.

The overlay adds the rest to `management.endpoints.web.exposure.include` and turns
on `health.show-details`, `env.show-values` and `configprops.show-values`.

`sc svc actuator-config` prints this same snippet, and `sc svc` points at it
whenever an endpoint is missing.

**Security.** `/env` and `/configprops` print resolved configuration values
including credentials; `/heapdump` writes a full heap image. In a shared
environment drop `env`, `configprops` and `heapdump` from the list — the rest of
the operator plane still works.

## lab/overlays/orca-local.yml — two orca fixes

### `tasks.controller.failedStages`

`TaskControllerConfigurationProperties` declares `failedStages` as a nested object
with no default instance, but `TaskController` dereferences
`configurationProperties.failedStages.onlyIncludeStagesThatFailPipelines`
unconditionally. `GET /pipelines/failedStages` therefore answers:

```
500 Cannot get property 'onlyIncludeStagesThatFailPipelines' on null object
```

on any installation that has not set the block — which is an upstream bug, not a
misconfiguration. Setting it enables the endpoint, including its descent into
nested pipeline executions.

`sc exec failed-stages` works either way: it recognizes this failure and derives
the failed stages from the execution instead, saying so.

### `user-configured-url-restrictions`

orca refuses SSRF-prone webhook targets by default. `excludedDomains` covers
`spinnaker`, `local`, `localdomain` and `internal`, and localhost and verbatim IPs
are rejected outright, so an in-cluster target such as
`front50.spinnaker.svc.cluster.local` fails with `Host not allowed`.

That is the right default for a real installation. The lab relaxes it so webhook
stages can target the installation's own services, which is what the E2E suite's
recovery test needs.

These property names are not in the documentation. They were found on the running
service with:

```bash
sc svc configprops orca restriction
```

## Using the lab

```bash
sc login --gate http://<host>/api/v1 \
    --user admin --password spinnakerSaysYouShouldChangeYourPassword \
    --context-name lab --save-namespace spinnaker --save-kubeconfig ~/.kube/config

sc doctor        # expect all OK
sc svc list      # keel is SCALED_TO_ZERO by design; everything else UP
```

`sc svc list` on a healthy lab:

```
SERVICE      REPLICAS  VERSION   HEALTH          PORT  ROLE
clouddriver  1/1       2026.3.0  UP              7002  cloud provider integration and the infrastructure c…
deck         1/1       2026.3.0  N/A             9000  static assets; no actuator
echo         1/1       2026.3.0  UP              8089  events, notifications and scheduled/webhook triggers
fiat         1/1       2026.3.0  UP              7003  authorization (roles, permissions)
front50      1/1       2026.3.0  UP              8080  persistent metadata: applications, pipelines, proje…
gate         1/1       2026.3.0  UP              8084  API gateway — the only externally exposed service
igor         1/1       2026.3.0  UP              8088  CI integration (Jenkins, Travis, Concourse, GitLab …
kayenta      1/1       2026.3.0  UP              8090  automated canary analysis
keel         0/0       2026.3.0  SCALED_TO_ZERO  7010  disabled by default upstream
orca         1/1       2026.3.0  UP              8083  orchestration: executes pipelines and tasks
rosco        1/1       2026.3.0  UP              8087  image bakery (Packer)
```

## Running the end-to-end suite

```bash
export SC_E2E=1
export SC_E2E_GATE=http://<host>/api/v1
export SC_E2E_USER=admin SC_E2E_PASSWORD=spinnakerSaysYouShouldChangeYourPassword
export SC_E2E_KUBECONFIG=~/.kube/config SC_E2E_NAMESPACE=spinnaker
export SC_E2E_ACCOUNT=managing SC_E2E_DEPLOY_NS=demo
export SC_E2E_WEBHOOK_URL=http://front50.spinnaker.svc.cluster.local:8080/health
kubectl create namespace demo
make e2e
```

Set `SC_E2E_KEEP=1` to leave the application and workload in place for inspection.
Application names should avoid hyphens: Spinnaker's Frigga naming convention reads
`app-stack-detail`.

## Notes on cluster compatibility

The lab has been exercised on **k3s v1.36** with Traefik and the local-path
provisioner. Two things to keep in mind on other clusters:

- `sc svc` uses the API server's `services/proxy` subresource, so the credentials
  need `get`/`create` on it in the namespace. A kubeconfig using an `exec`
  credential plugin (EKS, GKE) is rejected with a clear message — `sc` does not
  run credential plugins; use `--service-url` or a token-based kubeconfig.
- Clouddriver bundles its own kubectl. A large version skew between it and the API
  server can surface as manifest operations failing in clouddriver rather than in
  `sc`; `sc svc logs clouddriver` is where that shows up.
