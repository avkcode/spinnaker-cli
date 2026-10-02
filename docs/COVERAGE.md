# Gate API coverage

Gate 2026.3.0 exposes 73 controllers. This maps each to its `sc` command, or marks
it as reachable through the `sc api` escape hatch.

The inventory was taken from the controllers in
`gate-web/src/main/{groovy,java}/com/netflix/spinnaker/gate/controllers/` on the
2026.3.0 tag. To see what *your* installation actually serves — including
plugin-contributed routes — ask it:

```bash
sc svc mappings gate
```

**Legend** — ✅ dedicated command · 🔹 partially wrapped (common operations) ·
`sc api` reachable but not wrapped · ⬜ provider-specific, not applicable to a
Kubernetes-only install.

## Core: applications, pipelines, executions

| Controller | Coverage | Command |
|:--|:--:|:--|
| `ApplicationController` | ✅ | `sc app list/get/create/delete/history/resources` |
| `PipelineController` | ✅ | `sc pipeline apply/delete/rename/run`, `sc exec get/cancel/pause/resume/delete/restart-stage/eval` |
| `PipelineConfigController` | ✅ | `sc pipeline list/history/convert-to-template` |
| `ExecutionsController` | ✅ | `sc exec search/failed-stages`, `sc exec list` |
| `TaskController` | ✅ | `sc task list/get/submit/wait/cancel` |
| `V2PipelineTemplatesController` | 🔹 | `sc pipeline template list/get` (create/update via `sc api`) |
| `PipelineTemplatesController` (v1) | `sc api` | superseded by v2 |
| `StrategyConfigController` | 🔹 | `sc pipeline` (strategies are pipelines; `sc api` for CRUD) |
| `StrategyController` | `sc api` | |
| `ReorderPipelinesController` | 🔹 | `pkg/gate.ReorderPipelineConfigs` |
| `ProjectController` | ✅ | `sc project list/get` — `spin` never supported projects |
| `AdminController` | 🔹 | `sc exec cancel --force` (force-cancel a zombie) |

## Infrastructure

| Controller | Coverage | Command |
|:--|:--:|:--|
| `CredentialsController` | ✅ | `sc account list/get` |
| `ClusterController` | ✅ | `sc cluster list/get` |
| `ServerGroupController` | ✅ | `sc servergroup list/get` |
| `ServerGroupManagerController` | `sc api` | |
| `ManifestController` | ✅ | `sc manifest get` |
| `RawResourceController` | ✅ | `sc app resources` |
| `InstanceController` | 🔹 | `pkg/gate.GetInstance/GetInstanceConsole` |
| `LoadBalancerController` | 🔹 | `pkg/gate.ListLoadBalancers` |
| `SecurityGroupController` / `FirewallController` | 🔹 | `pkg/gate.ListSecurityGroups` |
| `SearchController` | ✅ | `sc search` (fans out over types, which Gate cannot) |
| `ImageController` | `sc api` | |
| `JobController` | `sc api` | |
| `SnapshotController` | `sc api` | |
| `NetworkController`, `SubnetController` | ⬜ | AWS/GCP |
| `CloudMetricController` | `sc api` | |
| `EntityTagsController`, `BatchEntityTagsController` | `sc api` | |
| `ServiceBrokerController` | `sc api` | Cloud Foundry |

## Artifacts, CI, bakery

| Controller | Coverage | Command |
|:--|:--:|:--|
| `ArtifactController` | ✅ | `sc artifact accounts/names/versions` |
| `BuildController` | 🔹 | `pkg/gate.ListBuildMasters/ListBuildJobs/ListBuilds/GetBuild` |
| `CiController` | 🔹 | `pkg/gate.ListCIBuilds` |
| `BakeController` | 🔹 | `pkg/gate.ListBakeOptions/GetBakeLogs` |
| `ArtifactoryController`, `NexusController` | `sc api` | |
| `ConcourseController` | `sc api` | |
| `AwsCodeBuildController`, `GoogleCloudBuildController` | ⬜ | AWS/GCP |
| `ChartImageController` | `sc api` | |

## Canary and managed delivery

| Controller | Coverage | Command |
|:--|:--:|:--|
| `V2CanaryController`, `V2CanaryConfigController` | 🔹 | `pkg/gate.ListCanaryConfigs/GetCanaryConfig/ListCanaryJudges/GetCanaryResult` |
| `CanaryController` (v1) | `sc api` | superseded by v2 |
| `ManagedController` (keel) | 🔹 | `pkg/gate.GetManagedApplication/GetDeliveryConfig/GetManagedResource` |

Kayenta and keel ship **disabled** upstream (`services.kayenta.enabled`,
`services.keel.enabled`). `sc svc list` reports them as deliberately disabled
rather than broken, and `sc svc` says so when a command targets one.

## Triggers, notifications, webhooks

| Controller | Coverage | Command |
|:--|:--:|:--|
| `WebhookController` | 🔹 | `pkg/gate.ListWebhookTypes/SendWebhook` |
| `CronController` | 🔹 | `pkg/gate.ListCronTriggers` |
| `NotificationController` | 🔹 | `pkg/gate.GetNotifications` |
| `PubsubSubscriptionController` | 🔹 | `pkg/gate.ListPubsubSubscriptions` |
| `SlackController`, `PagerDutyController` | `sc api` | |
| `GremlinController` | `sc api` | |

## Auth, identity, system

| Controller | Coverage | Command |
|:--|:--:|:--|
| `AuthController` | ✅ | `sc login`, `sc health` (`/auth/user`) |
| `LoginController` | ✅ | handled by the auth flow |
| `RoleController` | 🔹 | `pkg/gate.ListRoles/SyncRoles` |
| `ApiTokenController` | 🔹 | `sc login --token spk_…`; minting via `sc api` |
| `VersionController` | ✅ | `sc version`, `sc health` |
| `CapabilitiesController` | 🔹 | `pkg/gate.Capabilities` |
| `RootController` | ✅ | `sc health` |
| `CertificateController` | `sc api` | |
| `GlobalBannerController` | `sc api` | off unless `global-banner.enabled` |
| `DataController` | `sc api` | |
| `HistoryController` | 🔹 | `sc app history` |
| `CleanupController` | `sc api` | swabbie |
| `StorageAccountController` | `sc api` | |
| `CloudMetricController` | `sc api` | |

## Provider-specific

Not applicable to a Kubernetes-only installation; all reachable via `sc api`.

`AmazonInfrastructureController`, `EcsCloudMetricController`,
`EcsClusterController`, `EcsSecretsController`, `EcsServerGroupEventsController`,
`EcsServiceDiscoveryController`.

---

## Beyond Gate

The operator plane has no Gate equivalent at all — Gate proxies a curated subset
of what the services expose, and the endpoints below have no Gate route:

| Capability | Command |
|:--|:--|
| Per-service health with components | `sc svc health` |
| Resolved Spring configuration, with precedence | `sc svc env`, `sc svc configprops` |
| Live log-level changes | `sc svc set-level` |
| Metrics (queue depth, cache agents, JVM) | `sc svc metrics` |
| Thread-dump triage | `sc svc threads` |
| Live route inventory | `sc svc mappings` |
| Bean inventory | `sc svc beans` |
| Scheduled work | `sc svc scheduled` |
| Mounted configuration | `sc svc config` |
| Container logs | `sc svc logs` |
| Scale / restart / pods | `sc svc scale/restart/pods` |
| Any service endpoint | `sc svc api` |
