# AI Agent Source of Truth: sc (Spinnaker CLI)

Primary reference for agents and developers working on this repository.

## Purpose

`sc` is a standalone Go CLI for Spinnaker covering two planes: the Gate API (core)
and the individual microservices behind it (operator). It includes a stdio MCP
server with 42 tools.

Module: `github.com/avkcode/spinnaker-cli` · binary `sc` · Go 1.26.

## Architecture

```
main.go
 └─ cmd/          command files, all registered via init() on rootCmd
      ├─ root.go           global flags, context resolution, exit codes, audit
      │                    getGate()  → *gate.Client
      │                    getSvc()   → *svc.Client
      │                    getOutput()/render()
      ├─ config.go         login, logout, context, config
      ├─ app.go            applications
      ├─ pipeline.go       pipeline definitions, run, templates
      ├─ exec.go           executions, stages, SpEL, restart, follow
      ├─ judge.go          manual judgments
      ├─ task.go           ad-hoc orca tasks
      ├─ infra.go          accounts, clusters, server groups, manifests,
      │                    search, projects, artifacts
      ├─ api.go            Gate escape hatch
      ├─ svc.go            operator plane
      ├─ doctor.go         health, doctor, version
      ├─ mcp.go            MCP server + core tools
      ├─ mcp_tools_exec.go execution/task/judgment tools
      ├─ mcp_tools_operator.go  operator-plane tools
      ├─ mcp_resources.go  MCP resources and prompts
      ├─ util.go           tables, field extraction, formatting, input
      └─ prompt.go         interactive confirmation
 └─ pkg/
      ├─ gate/      Gate API client (client.go + one file per domain)
      ├─ svc/       per-service access (svc.go) + actuator (actuator.go)
      ├─ k8s/       minimal Kubernetes client (kubeconfig.go, client.go)
      ├─ mcp/       MCP protocol server
      └─ output/    table/json/yaml writer
 └─ lab/           reproducible Spinnaker lab on Kubernetes
 └─ test/e2e/      end-to-end suite driving the built binary
```

## Conventions

### Adding a command

1. Create `cmd/feature.go` in package `cmd`.
2. Declare a `*cobra.Command` with a `RunE` handler.
3. Register in `init()` on its parent (or `rootCmd.AddCommand`).
4. Set `GroupID`: `GroupCore`, `GroupOperator` or `GroupConfig` — a command with
   no group lands under "Additional Commands" in help.
5. Use `getGate()` / `getSvc()`, `cmdContext(cmd.Context())`, and
   `outputIsStructured()` + `render()` for `-o json|yaml`.
6. Honour `isDryRun()` on anything that mutates, and call `audit()` after it.
7. Add `ValidArgsFunction` where completion helps (applications, pipelines,
   accounts, services).

### Adding a Gate client method

1. Put it in `pkg/gate/<domain>.go` on `*Client`.
2. Build paths with `escape()` per segment — never interpolate a raw name.
   Use `escapeApplication()` for an application segment that may be `*`.
3. Return `JSONMap` / `JSONList` for pass-through models. Gate hands back a
   downstream service's model largely untyped and different providers populate
   different fields; inventing structs makes the client drift from the server.
   Define a struct only where the shape is stable and the field names matter
   (see `FailedStage`).
4. Take `context.Context` first.

### Output pattern

```go
if outputIsStructured() {
    return render(v)
}
t := newTable("NAME", "STATUS")
t.add(name, status)
t.print("Nothing found.")   // the note goes to stderr when there are no rows
```

Field extraction helpers in `util.go` (`str`, `strOr`, `num`, `boolean`,
`mapField`, `listField`, `mapList`) read loosely-typed JSON without repeating type
switches. Formatting helpers: `epochTime`, `age`, `execDuration`,
`shortDuration`, `ellipsis`, `dash`.

### Timeouts

`--timeout` bounds a single API request. Operations that legitimately outlast one
— following an execution, waiting on a task, streaming logs — must use
`cmd.Context()` directly and take their own `--wait-timeout`. Wrapping a
multi-minute wait in `cmdContext` is a bug.

### Flags

Global: `--gate --user --password --token --cert --key --output --insecure
--timeout --dry-run --log-level --context --namespace --kubeconfig --kube-context
--service-url`.

Shorthands are taken: `-u` user, `-p` password, `-t` token, `-o` output,
`-k` insecure, `-n` namespace, `-f` file, `-P` pipeline parameter, `-H` header.

### Exit codes

Defined in `cmd/root.go`. `classifyError` maps errors to them. Return
`&ExecFailedError{}` when the CLI worked but its subject failed — that becomes
exit 8, which is what CI branches on.

## MCP server

Tools are registered in `cmd/mcp_tools_*.go`. Each has `Name`, `Description`,
`InputSchema` and a `Handler`, plus safety flags:

- `ReadOnly` — does not mutate.
- `Destructive` — may delete or disrupt.
- `Idempotent` — repeating has the same effect.
- `Script` — reaches past the Gate API into service internals, or mutates the
  cluster. Gated behind `--allow-script`.

Use `schemaWithContext` / `mutatingSchema` so every tool accepts `context` (and
`dryRun` for mutations). Connect with `mcpGate` / `mcpSvc`. Return
`mcp.Errorf(code, …)` for argument errors so the client gets a parseable payload.

Descriptions are the agent's only documentation — say what the tool is *for* and
when to prefer it, not just what it wraps.

## Spinnaker behavior worth knowing

These were established against a live 2026.3.0 installation; several are not
documented anywhere.

- **Gate sits under a context path.** The upstream kustomize install mounts it at
  `/api/v1` behind one ingress. The configured endpoint must include it.
- **Actuator lives at the service root.** kork's
  `SpringBoot1CompatibilityApplicationListener` sets
  `management.endpoints.web.base-path=/` for `/health` compatibility, so endpoints
  are `/env`, `/loggers`, … not `/actuator/*`. Spring Boot exposes only `/health`
  by default.
- **Gate authenticates its own actuator endpoints** — everything but `/health`
  redirects to its login page. The Kubernetes API-server proxy replaces the
  Authorization header with its own, so `pkg/svc` reaches gate through the
  configured Gate URL with the user's credentials instead.
- **`*` works on exactly one endpoint.**
  `/applications/{app}/executions/search` accepts it;
  `/applications/{app}/pipelines` answers 400. And it must stay *literal* — Spring
  matches the raw segment, so `%2A` fails.
- **Pre-escaped paths must not go into `url.URL.Path`.** That field is the decoded
  path, so `URL.String()` escapes it again and `%20` becomes `%2520`. Both clients
  use `setEscapedPath` to set `RawPath` alongside `Path`.
- **Spring serializes non-finite doubles as JSON strings** (`"NaN"`,
  `"Infinity"`), so an actuator measurement cannot decode into `float64`.
- **The restart-stage body is not a context override.** orca passes it only to
  `updatePreconditionStageExpression`, which rewrites `preconditions` on
  `checkPreconditions` stages. To correct a value, PATCH the stage context first —
  orca's restart resets status, times and tasks but *preserves* the context, and
  records the old failure under `restartDetails.previousException`. That is what
  `RestartStageWithOverrides` does.
- **Restart is queued, not immediate.** For a moment after it, the execution still
  reports its previous terminal status; following straight away reports the old
  failure. See `waitForRestart`.
- **front50 has two read paths with different freshness.** A direct application
  lookup is immediate; the collection behind `app list` is served from a cache
  that refreshes on its own cycle. `app create --wait` waits for direct
  readability; a listing may lag by up to a minute.
- **`tasks.controller.failedStages` is a nested config object with no default**,
  dereferenced unconditionally by orca's `TaskController`, so
  `/executions/failedStages` answers 500 until it is set. `sc` detects this and
  derives failed stages from the execution instead.
- **orca blocks in-cluster webhook targets** by default:
  `user-configured-url-restrictions.excludedDomains` covers `spinnaker`, `local`,
  `localdomain` and `internal`, and localhost and verbatim IPs are rejected.
- **Pod logs take no Accept header.** The API server content-negotiates the log
  subresource against its own serializers and answers 406 to `text/plain`.
- **Application names flow through Frigga**, which reads `app-stack-detail`, so a
  hyphen in an application name is best avoided.

## Testing

- `make test` — unit tests, hermetic (`httptest` servers, temp kubeconfigs).
- `make e2e` — drives the built binary against a real installation; skipped
  unless `SC_E2E=1` and `SC_E2E_GATE` are set. See `test/e2e/e2e_test.go` for the
  full environment.
- `make lint fmt-check` — golangci-lint and gofmt.

Go runs top-level tests in **source order**, not alphabetically. Suite-wide
teardown belongs in `TestMain` after `m.Run()`, not in a test named to sort last.

## The lab

`lab/setup.sh` deploys the upstream kustomize base pinned to a release tag, plus
`lab/overlays/`. Halyard is deprecated; kustomize is the supported path. See
`docs/LAB.md`.
