# sc — Spinnaker CLI

A standalone Go CLI for Spinnaker that covers **two planes**:

| Plane | What it is | What it answers |
|:--|:--|:--|
| **Core** | the Gate API — applications, pipelines, executions, tasks, judgments, infrastructure | what a user or a pipeline does |
| **Operator** | the services *behind* Gate — orca, clouddriver, front50, igor, echo, fiat, rosco, kayenta, keel | what an operator does |

Both planes have an escape hatch (`sc api`, `sc svc api`), so anything not wrapped
is still one command away.

## Why this exists

Spinnaker's own tooling has thinned out:

- **`spin`**, the official CLI, was archived into the monorepo and last released
  v1.30.0 in April 2023. It is a Swagger-generated client covering eight command
  families (applications, pipelines, pipeline executions, pipeline templates,
  projects, accounts, canary configs, orca tasks) with authentication-only
  configuration — no streaming, no diffing, no judgments, no deployments, no
  operator surface.
- **Halyard** was fully deprecated in Spinnaker 2026.1.0, in favor of the
  kustomize install. Nothing replaced its operational half.
- **Gate 2026.3.0 ships an embedded MCP server** (`gate-mcp`), which covers the
  user plane well. It is off by default and, living inside Gate, can only see
  what Gate can see.

The gap this fills is the operator/platform plane. Gate's API cannot tell you that
orca's queue is backing up, which configuration clouddriver actually resolved at
startup, or which log category to turn up to find out why a deploy stalled. Those
answers live on each service's own port, and `sc svc` is how you get them.

## Install

```bash
go install github.com/avkcode/spinnaker-cli@latest   # as 'spinnaker-cli'
# or build with the binary name:
git clone https://github.com/avkcode/spinnaker-cli && cd spinnaker-cli
make build && sudo make install                       # installs /usr/local/bin/sc
```

## Quick start

```bash
# Gate is usually mounted under a servlet context path — include it.
sc login --gate http://spinnaker.example.com/api/v1 --user admin --password secret

sc health                 # reachability, version, identity, roles, accounts
sc doctor                 # full diagnosis across both planes
sc svc list               # every service: replicas, image version, health
```

`login` verifies the credentials against `/auth/user` before saving, and reports
the Fiat roles the identity actually has.

### Multiple installations

```bash
sc context set prod --gate https://spinnaker.example.com/api/v1 --token spk_abc
sc context set lab  --gate http://lab.example.com/api/v1 --user admin --password pw \
                    --namespace spinnaker --kubeconfig ~/.kube/lab
sc context use lab
sc context list
sc --context prod app list     # one-off override
```

Authentication: basic auth, a Gate API token (`spk_…`, sent as
`X-Spinnaker-Token`), an OAuth2/OIDC bearer token, or x509 (`--cert/--key`).

---

## Core plane

### Applications

```bash
sc app list                          # registered + clouddriver-inferred
sc app list --registered             # only those with front50 metadata
sc app get demo
sc app create demo --email me@example.com --cloud-providers kubernetes --wait
sc app delete demo
sc app history demo
sc app resources demo                # cached ConfigMaps, Secrets, CRDs
```

There is no REST "create application" endpoint — writes go through orca as an
`upsertApplication` task, exactly as Deck does. `--wait` blocks until that task
finishes *and* until the application is actually readable, because Gate serves
applications from a front50 cache that refreshes on its own cycle.

### Pipelines as code

```bash
sc pipeline list demo
sc pipeline get demo deploy > deploy.json       # export
sc pipeline apply -f deploy.json                # import (id ⇒ update)
sc pipeline apply -f all.json --bulk            # one call for a whole sync
sc pipeline get demo deploy | sc pipeline apply -f - --application demo2
sc pipeline rename demo deploy deploy-v2        # keeps the id, so keeps history
sc pipeline disable demo deploy
sc pipeline delete demo deploy
```

### Running and watching

```bash
sc pipeline run demo deploy -P BRANCH=main -P REPLICAS=3
sc pipeline run demo deploy --follow            # render stages as they change
sc pipeline run demo deploy --wait              # exit 8 if it fails → CI gate
```

### Executions

```bash
sc exec list demo --status RUNNING
sc exec stages 01M3Y...                         # where it stands, and why
sc exec failed-stages 01M3Y...                  # just the failures
sc exec wait 01M3Y... --wait-timeout 10m        # exit 8 on failure
sc exec pause / resume / cancel 01M3Y...
sc exec search --application '*' --status RUNNING --since 24h
sc exec delete 01M3Y...
```

**SpEL evaluation** — the closest thing Spinnaker has to a read-only script
console. It answers what a stage *actually resolved*, not what the definition
said it would:

```bash
sc exec eval 01M3Y... '${trigger.parameters}'
sc exec eval 01M3Y... '${#stage("deploy").context.manifests}' --stage deploy
sc exec eval 01M3Y... '${execution.stages.?[status == "TERMINAL"].![name]}'
```

**Restart one stage**, optionally correcting a value first:

```bash
sc exec restart-stage 01M3Y... deploy-prod
sc exec restart-stage 01M3Y... deploy-prod --set url=https://fixed.example.com --follow
```

The execution keeps its id and history, so a long pipeline that failed at stage
nine does not repeat stages one to eight. `--set` patches the stage's stored
context before the restart — orca preserves that context across a restart and
files the original failure under `restartDetails.previousException`. (The restart
endpoint's own body is *not* a general override: orca only applies it to
`checkPreconditions` stages.)

### Manual judgments

A pipeline paused on a judgment is holding a deployment open, so finding them
quickly matters. Gate has no endpoint for this; `sc` derives it.

```bash
sc judge list                                   # across all applications
sc judge get 01M3Y...
sc judge continue 01M3Y... "Approve deploy" --input proceed
sc judge stop 01M3Y...
```

### Tasks — the write-side escape hatch

Every imperative Spinnaker action is an ad-hoc orca task, so this reaches
operations that have no dedicated command:

```bash
sc task list demo
sc task get 01M3Y...
echo '[{"type":"disableServerGroup","serverGroupName":"demo-v001",
        "region":"default","credentials":"managing"}]' \
  | sc task submit -f - --application demo --wait
```

### Infrastructure

```bash
sc account list                                 # and 'sc account get managing'
sc cluster list demo
sc cluster get demo managing demo-nginx
sc servergroup list demo
sc manifest get managing default "deployment nginx"
sc search nginx --type serverGroups --type clusters
sc project list
sc artifact accounts
```

`sc manifest get` shows the status Spinnaker *computed* for a resource — which is
exactly what a "wait for manifest to stabilize" stage is waiting on.

### Escape hatch

```bash
sc api GET /applications
sc api GET /executions --query pipelineConfigIds=abc --query limit=5
sc api POST /webhooks/webhook/my-trigger --body '{"parameters":{"branch":"main"}}'
sc api PUT /pipelines/01M3Y.../cancel
sc api GET /applications --raw | jq '.[].name'
```

---

## Operator plane

Requests reach each service through the **Kubernetes API server's Service proxy**,
so this needs only the credentials kubectl already has — no port-forward
subprocess, no exposed ports, and it works from outside the cluster.
`--service-url NAME=URL` bypasses Kubernetes for non-Kubernetes installs.

```bash
sc svc list                 # replicas, image version, health, in one table
sc svc health orca          # per-component: db, redis, downstream services
sc svc pods clouddriver
sc svc logs orca --follow
sc svc scale keel 1
sc svc restart orca         # needed after a ConfigMap change
```

### What configuration is this service *actually* using?

```bash
sc svc env orca redis               # resolved values, in precedence order
sc svc env clouddriver kubernetes
sc svc env orca --profiles          # active Spring profiles
sc svc configprops clouddriver kubernetes   # the typed @ConfigurationProperties view
sc svc config orca                  # the ConfigMaps mounted into it
```

`sc svc env` marks shadowed values `OVERRIDDEN`, which is normally the entire
explanation when a config change appears to have had no effect.

### Change a log level with no restart

```bash
sc svc loggers orca com.netflix.spinnaker.orca
sc svc set-level clouddriver com.netflix.spinnaker.clouddriver.kubernetes DEBUG
sc svc logs clouddriver --follow
sc svc set-level clouddriver com.netflix.spinnaker.clouddriver.kubernetes ""   # reset
```

This turns an investigation that would need a redeploy into one command. The
change lives in the running JVM and reverts on its next restart.

### Is it wedged? Where are the queues?

```bash
sc svc threads orca                 # state counts + every blocked thread
sc svc metrics orca queue           # queue.depth, queue.message.lag, …
sc svc metrics orca jvm.memory.used --tag area:heap
sc svc scheduled echo               # trigger polling / cache agent schedules
```

### Discover what a running service actually serves

```bash
sc svc mappings gate pipelines      # authoritative endpoint inventory
sc svc mappings clouddriver manifests
sc svc beans orca ManualJudgment    # did that optional component load?
```

More reliable than documentation: it reflects exactly what this build and
configuration expose, including plugin-contributed routes.

### Reach past Gate entirely

```bash
sc svc api orca GET /pipelines/01M3Y...          # raw execution document
sc svc api clouddriver GET /cache/introspection  # cache agent state
sc svc api front50 GET /v2/applications          # unfiltered collection
sc svc api igor GET /masters
```

These are internal endpoints: not a stable contract, and mostly enforcing no Fiat
authorization of their own, so this bypasses the checks Gate would apply. Gate's
own actuator endpoints *are* authenticated, and `sc` routes them through the
configured Gate URL with your credentials, since the API-server proxy cannot carry
them.

### Enabling it

kork mounts actuator at the service **root** (not `/actuator`) so `/health` stays
where it has always been, and Spring Boot exposes only `/health` by default:

```bash
sc svc actuator-config > overlays/config/files/spinnaker-local.yml
sc svc restart orca     # services read configuration only at startup
```

`sc svc` says exactly this when an endpoint is missing, rather than reporting a
bare 404.

---

## Diagnosis

```bash
sc doctor                              # the installation
sc doctor --execution 01M3Y...         # one failed execution
```

`doctor` checks Gate reachability and identity, accounts, every service's
deployment state and health, and whether the operator plane is exposed — then
reports `OK`/`WARN`/`FAIL` with a concrete next step. It exits **8** when it finds
problems. On an execution it names the failing stage, its exception, and the
`restart-stage` command to retry it:

```
      AREA             FINDING
FAIL  execution        demo/failing-webhook is TERMINAL (391ms)
FAIL  stage/Call hook  TERMINAL after 276ms: URI not valid: http://no-such-host.invalid/hook
                       → sc exec restart-stage 01M3Y... Call hook
OK    orca             reports UP
OK    clouddriver      reports UP
```

---

## AI agent integration (MCP)

`sc mcp` is a stdio Model Context Protocol server with **42 tools**, 6 resource
templates and 3 prompts.

```bash
sc mcp                  # read + mutating tools
sc mcp --read-only      # 20 read-only tools
sc mcp --allow-script   # + tools that reach service internals or mutate the cluster
```

```jsonc
// .mcp.json / ~/.config/opencode/opencode.jsonc
{
  "mcpServers": {
    "spinnaker": {
      "command": "sc",
      "args": ["mcp", "--allow-script"]
    }
  }
}
```

**Relationship to Gate's own MCP server.** Gate 2026.3.0 embeds one (`gate-mcp`)
covering the user plane. This server is the complement, not a duplicate: it works
whether or not `mcp.server.enabled` is set, and its distinctive tools are the
operator-plane ones that no Gate endpoint can answer — `service_config`,
`set_log_level`, `service_thread_summary`, `inspect_service_routes`,
`call_service_api`, `service_logs`, `scale_service`, `diagnose`.

| Safety flag | Effect |
|:--|:--|
| *(default)* | read + mutating Gate tools; service-internal tools disabled |
| `--read-only` | read-only tools only |
| `--allow-script` | also enables tools that bypass Gate or mutate the cluster |

Prompts: `triage-failed-execution`, `review-manual-judgment`,
`installation-report`.

---

## Exit codes

| Code | Meaning |
|--:|:--|
| 0 | success |
| 1 | authentication / authorization |
| 2 | network |
| 3 | timeout |
| 4 | configuration (kubeconfig, context) |
| 5 | not found |
| 6 | conflict |
| 7 | usage |
| **8** | **the execution or task itself failed** (the CLI worked) |
| 99 | internal |

Code 8 is the one CI should branch on: it separates "your deployment failed" from
"the CLI broke".

---

## The lab

`lab/setup.sh` stands up a complete Spinnaker on any Kubernetes cluster, for
development and end-to-end testing. It deploys the upstream kustomize base — the
supported path since Halyard's deprecation — pinned to a release tag, plus the two
overlays in `lab/overlays/` that the operator plane and the E2E suite need.

```bash
lab/setup.sh                      # ~16GB RAM, 6 cores; private clusters only
lab/setup.sh --delete
```

See [docs/LAB.md](docs/LAB.md) for what it configures and why.

## Testing

```bash
make test        # unit tests, hermetic
make e2e         # end-to-end against a real installation
make lint fmt-check
```

The E2E suite drives the built binary against a live Spinnaker: it creates an
application, defines a pipeline with a manual judgment and a Kubernetes deploy,
triggers it, answers the judgment, confirms the workload stabilises, then breaks a
pipeline on purpose and verifies the whole triage loop (`doctor` → `restart-stage
--set` → success), the operator plane, the MCP server, exit codes and `--dry-run`.
See [test/e2e/e2e_test.go](test/e2e/e2e_test.go).

## Architecture

```
main.go
 └─ cmd/                  cobra commands, registered via init()
     ├─ root.go           global flags, contexts, exit codes, audit log
     ├─ config.go         login/logout/context/config
     ├─ app.go            applications
     ├─ pipeline.go       pipeline definitions + run
     ├─ exec.go           executions, stages, SpEL, restart, follow
     ├─ judge.go          manual judgments
     ├─ task.go           ad-hoc orca tasks
     ├─ infra.go          accounts, clusters, server groups, manifests, search,
     │                    projects, artifacts
     ├─ api.go            Gate escape hatch
     ├─ svc.go            the operator plane
     ├─ doctor.go         health, doctor, version
     ├─ mcp*.go           MCP server: tools, resources, prompts
     └─ util.go           tables, field extraction, formatting, input
 └─ pkg/
     ├─ gate/             Gate API client, one file per domain
     ├─ svc/              per-service access + Spring Boot actuator
     ├─ k8s/              minimal Kubernetes client (no client-go)
     ├─ mcp/              MCP protocol server
     └─ output/           table/json/yaml rendering
```

Dependencies are deliberately few: cobra, viper, yaml.v3, golang.org/x/term. The
Kubernetes client is hand-rolled against the four REST verbs the operator plane
needs, rather than pulling in client-go's dependency tree for a CLI.

## Coverage

[docs/COVERAGE.md](docs/COVERAGE.md) maps every Gate controller to its command, or
notes that `sc api` covers it.

## Contributing

1. Trunk-based development on `main`.
2. New commands: `cmd/<name>.go`, registered in `init()`.
3. New Gate methods: `pkg/gate/<domain>.go`.
4. New MCP tools: `cmd/mcp_tools_*.go`.
5. `make fmt-check test lint` before pushing.

See [AGENTS.md](AGENTS.md) for the conventions in detail.
