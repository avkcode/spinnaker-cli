package cmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/k8s"
	scsvc "github.com/avkcode/spinnaker-cli/pkg/svc"
	"github.com/spf13/cobra"
)

var svcCmd = &cobra.Command{
	Use:     "svc",
	Aliases: []string{"service", "services"},
	Short:   "Operate on the Spinnaker microservices behind Gate",
	Long: `Operate on the individual Spinnaker microservices.

Gate's API is the user plane: applications, pipelines, executions. It cannot tell
you that orca's queue is backing up, which configuration clouddriver actually
resolved at startup, or which log category to turn up to find out why a deploy
stalled. Those answers live on each service's own port.

Transport. By default requests go through the Kubernetes API server's Service
proxy, so this needs only the credentials kubectl already has — no port-forward, no
exposed ports, and it works from outside the cluster. --service-url NAME=URL
bypasses Kubernetes for installs that are not on it.

Exposure. kork mounts actuator endpoints at the service ROOT (not /actuator) so
that /health stays where it has always been. Spring Boot exposes only /health by
default, so most commands here need the other endpoints enabled first — run
'sc svc actuator-config' for the configuration to apply.`,
	GroupID: GroupOperator,
}

var svcListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "ps"},
	Short:   "Show every service's replicas, image version and health",
	Long: `Shows each Spinnaker service's Deployment state next to its actuator health.

This is the first command to run against an unfamiliar or misbehaving
installation: it distinguishes "not deployed" from "deployed but unhealthy" from
"deliberately disabled", which the Kubernetes and Gate views each only half
answer.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		statuses, err := client.List(ctx)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(statuses)
		}
		t := newTable("SERVICE", "REPLICAS", "VERSION", "HEALTH", "PORT", "ROLE")
		for _, s := range statuses {
			role := s.Role
			if s.Note != "" {
				role = s.Note
			}
			t.add(s.Name, s.Replicas, dash(s.Version), s.Health, fmt.Sprint(s.Port), ellipsis(role, 52))
		}
		t.print("No Spinnaker services found in namespace " + client.Namespace() + ".")
		return nil
	},
}

var svcHealthCmd = &cobra.Command{
	Use:   "health [service...]",
	Short: "Show services' actuator health, including per-component detail",
	Long: `Shows a service's actuator health document.

With management.endpoint.health.show-details=always this includes each component's
status — redis, the SQL connection, downstream services — which is what turns
liveness into a diagnosis.`,
	Example: `  sc svc health
  sc svc health orca
  sc svc health orca clouddriver -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		targets := args
		if len(targets) == 0 {
			targets = scsvc.JVMNames()
		}

		type componentRow struct {
			Service   string `json:"service" yaml:"service"`
			Component string `json:"component" yaml:"component"`
			Status    string `json:"status" yaml:"status"`
			Detail    string `json:"detail,omitempty" yaml:"detail,omitempty"`
		}
		rows := []componentRow{}
		structured := map[string]any{}
		var mu sync.Mutex
		var wg sync.WaitGroup

		for _, name := range targets {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				health, err := client.Health(ctx, name)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					rows = append(rows, componentRow{Service: name, Component: "-", Status: "UNREACHABLE", Detail: ellipsis(err.Error(), 70)})
					structured[name] = map[string]any{"error": err.Error()}
					return
				}
				structured[name] = health
				status, _ := health["status"].(string)
				rows = append(rows, componentRow{Service: name, Component: "(overall)", Status: status})
				components, _ := health["components"].(map[string]any)
				for _, key := range sortedKeys(components) {
					comp, _ := components[key].(map[string]any)
					cstatus, _ := comp["status"].(string)
					detail := ""
					if d, ok := comp["details"].(map[string]any); ok && len(d) > 0 {
						parts := []string{}
						for _, dk := range sortedKeys(d) {
							parts = append(parts, fmt.Sprintf("%s=%v", dk, d[dk]))
						}
						detail = strings.Join(parts, " ")
					}
					rows = append(rows, componentRow{Service: name, Component: key, Status: cstatus, Detail: ellipsis(detail, 60)})
				}
			}(name)
		}
		wg.Wait()

		if outputIsStructured() {
			return render(structured)
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Service != rows[j].Service {
				return rows[i].Service < rows[j].Service
			}
			// Keep the overall status first within each service.
			if (rows[i].Component == "(overall)") != (rows[j].Component == "(overall)") {
				return rows[i].Component == "(overall)"
			}
			return rows[i].Component < rows[j].Component
		})
		t := newTable("SERVICE", "COMPONENT", "STATUS", "DETAIL")
		for _, r := range rows {
			t.add(r.Service, r.Component, r.Status, dash(r.Detail))
		}
		t.print("No health information available.")
		return nil
	},
	ValidArgsFunction: completeServices,
}

var (
	svcEnvFilter   string
	svcEnvAll      bool
	svcEnvProfiles bool
)

var svcEnvCmd = &cobra.Command{
	Use:   "env [service] [filter]",
	Short: "Show a service's resolved Spring configuration",
	Long: `Shows the configuration a service actually resolved, in precedence order.

This answers the question a config file cannot: not "what did I write" but "what
value is this service using, and which source won". Shadowed values are marked
OVERRIDDEN, which is normally the whole explanation when an edit appears to have
had no effect.

Values can include credentials, so this command is only as safe as the exposure
you have configured.`,
	Args: cobra.RangeArgs(1, 2),
	Example: `  sc svc env orca redis
  sc svc env clouddriver kubernetes
  sc svc env front50 --all -o yaml
  sc svc env orca --profiles`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		service := args[0]
		if svcEnvProfiles {
			profiles, err := client.ActiveProfiles(ctx, service)
			if err != nil {
				return err
			}
			if outputIsStructured() {
				return render(map[string]any{"service": service, "activeProfiles": profiles})
			}
			fmt.Printf("%s active profiles: %s\n", service, strings.Join(profiles, ", "))
			return nil
		}

		filter := svcEnvFilter
		if len(args) == 2 {
			filter = args[1]
		}
		if filter == "" && !svcEnvAll {
			return fmt.Errorf("a filter is required (a service resolves thousands of properties); pass a filter or --all")
		}

		props, err := client.Env(ctx, service, filter)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(props)
		}
		t := newTable("PROPERTY", "VALUE", "SOURCE", "")
		for _, p := range props {
			marker := ""
			if p.Overridden {
				marker = "OVERRIDDEN"
			}
			t.add(p.Name, ellipsis(p.Value, 44), ellipsis(p.Source, 40), marker)
		}
		t.print("No properties matched " + filter + ".")
		return nil
	},
	ValidArgsFunction: completeServices,
}

var (
	svcLoggersConfigured bool
)

var svcLoggersCmd = &cobra.Command{
	Use:   "loggers [service] [filter]",
	Short: "List a service's log categories and levels",
	Long: `Lists a service's log categories with their configured and effective levels.

--configured shows only categories someone has explicitly set, which is the quick
way to find log levels left turned up after an incident.`,
	Args: cobra.RangeArgs(1, 2),
	Example: `  sc svc loggers orca com.netflix.spinnaker.orca
  sc svc loggers clouddriver --configured`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		filter := ""
		if len(args) == 2 {
			filter = args[1]
		}
		if filter == "" && !svcLoggersConfigured {
			// Unfiltered this is thousands of rows of noise.
			filter = "com.netflix.spinnaker"
			fmt.Fprintf(os.Stderr, "No filter given; showing %q. Pass a filter or --configured to change this.\n", filter)
		}
		loggers, err := client.Loggers(ctx, args[0], filter, svcLoggersConfigured)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(loggers)
		}
		t := newTable("LOGGER", "CONFIGURED", "EFFECTIVE")
		for _, l := range loggers {
			t.add(l.Name, dash(l.ConfiguredLevel), l.EffectiveLevel)
		}
		t.print("No log categories matched.")
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcSetLevelCmd = &cobra.Command{
	Use:   "set-level [service] [logger] [level]",
	Short: "Change a log level on a running service, with no restart",
	Long: `Changes a log category's level on a running service.

No restart, no config change, no redeploy — the JVM's logging context is updated
in place and reverts on its next restart. This turns a reproduction that needed a
redeploy into one that needs a single command, and it is the highest-value write
in the operator plane.

Pass an empty level ("") to reset a category to inherit from its parent.

Valid levels: TRACE, DEBUG, INFO, WARN, ERROR, FATAL, OFF.`,
	Args: cobra.ExactArgs(3),
	Example: `  sc svc set-level orca com.netflix.spinnaker.orca DEBUG
  sc svc set-level clouddriver com.netflix.spinnaker.clouddriver.kubernetes TRACE
  sc svc set-level orca com.netflix.spinnaker.orca ""      # reset to inherited`,
	RunE: func(cmd *cobra.Command, args []string) error {
		service, logger, level := args[0], args[1], args[2]
		client, err := getSvc()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would set %s logger %s to %s", service, logger, dash(level))
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := client.SetLogLevel(ctx, service, logger, level); err != nil {
			return err
		}
		audit("svc.set-level", fmt.Sprintf("%s %s=%s", service, logger, level))

		// Report the level that actually took effect, since a parent category can
		// still dominate after a reset.
		result, err := client.GetLogger(ctx, service, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Set %s logger %s to %s\n", service, logger, dash(level))
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s logger %s: configured=%s effective=%s\n",
			service, logger, dash(result.ConfiguredLevel), result.EffectiveLevel)
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcMetricsCmd = &cobra.Command{
	Use:   "metrics [service] [metric]",
	Short: "List metric names, or read one metric",
	Long: `Lists the metric names a service publishes, or reads one metric's measurements.

Useful ones: queue depth and lag on orca (queue.*), cache agent timings on
clouddriver (executionTime, cats.*), HTTP latency anywhere (http.server.requests),
and JVM memory/GC everywhere (jvm.*).`,
	Args: cobra.RangeArgs(1, 2),
	Example: `  sc svc metrics orca queue
  sc svc metrics orca jvm.memory.used
  sc svc metrics clouddriver http.server.requests --tag status:500`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		if len(args) == 1 {
			names, err := client.MetricNames(ctx, args[0], "")
			if err != nil {
				return err
			}
			if outputIsStructured() {
				return render(names)
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return nil
		}

		// A second argument is either an exact metric name or a filter; try it as
		// a name first, since that is what the user usually means.
		metric, err := client.Metric(ctx, args[0], args[1], svcMetricTags)
		if err == nil {
			if outputIsStructured() {
				return render(metric)
			}
			w := getOutput()
			tw := w.Table()
			fmt.Fprintf(tw, "Metric:\t%s\n", metric.Name)
			if metric.Description != "" {
				fmt.Fprintf(tw, "Description:\t%s\n", metric.Description)
			}
			if metric.BaseUnit != "" {
				fmt.Fprintf(tw, "Unit:\t%s\n", metric.BaseUnit)
			}
			for _, k := range sortedKeys(metric.Measurements) {
				fmt.Fprintf(tw, "%s:\t%v\n", k, metric.Measurements[k])
			}
			w.FlushTable(tw)
			if len(metric.AvailableTags) > 0 {
				fmt.Println()
				tt := newTable("TAG", "VALUES")
				for _, k := range sortedKeys(metric.AvailableTags) {
					tt.add(k, ellipsis(strings.Join(metric.AvailableTags[k], ","), 70))
				}
				tt.print("")
			}
			return nil
		}

		names, nerr := client.MetricNames(ctx, args[0], args[1])
		if nerr != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("no metric named or matching %q on %s", args[1], args[0])
		}
		if outputIsStructured() {
			return render(names)
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcMetricTags []string

var svcThreadsFrames int

var svcThreadsCmd = &cobra.Command{
	Use:     "threads [service]",
	Aliases: []string{"threaddump"},
	Short:   "Summarize a thread dump, highlighting blocked threads",
	Long: `Summarizes a service's thread dump: thread counts by state, plus every blocked
thread with the lock it is waiting on and its top frames.

This is what identifies a wedged service — an orca that has stopped draining its
queue, or a clouddriver stuck in a provider call — without reading a
several-megabyte dump by hand. Use --raw for the full dump.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		if svcThreadsRaw {
			raw, err := client.ThreadDump(ctx, args[0])
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(prettyJSON(raw))
			return err
		}

		summary, err := client.ThreadSummary(ctx, args[0], svcThreadsFrames)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(summary)
		}
		w := getOutput()
		tw := w.Table()
		fmt.Fprintf(tw, "Total threads:\t%d\n", summary.Total)
		for _, state := range sortedKeys(summary.ByState) {
			fmt.Fprintf(tw, "%s:\t%d\n", state, summary.ByState[state])
		}
		w.FlushTable(tw)

		if len(summary.Blocked) == 0 {
			fmt.Fprintln(os.Stderr, "\nNo blocked threads.")
			return nil
		}
		fmt.Printf("\n%d blocked thread(s):\n", len(summary.Blocked))
		for _, th := range summary.Blocked {
			fmt.Printf("\n  %s (id %d)\n", th.Name, th.ID)
			if th.LockName != "" {
				fmt.Printf("    waiting on: %s\n", th.LockName)
			}
			if th.LockOwner != "" {
				fmt.Printf("    held by:    %s\n", th.LockOwner)
			}
			for _, frame := range th.Top {
				fmt.Printf("      at %s\n", frame)
			}
		}
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcThreadsRaw bool

var svcConfigPropsCmd = &cobra.Command{
	Use:     "configprops [service] [filter]",
	Aliases: []string{"props"},
	Short:   "Show a service's bound @ConfigurationProperties",
	Long: `Shows a service's @ConfigurationProperties beans with their bound values — the
typed view of configuration, as opposed to 'sc svc env's flat property list.

This is the reliable way to see what a structured config block resolved to, for
example which Kubernetes accounts clouddriver ended up with and with what
settings.`,
	Args: cobra.RangeArgs(1, 2),
	Example: `  sc svc configprops clouddriver kubernetes
  sc svc configprops orca tasks -o yaml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		filter := ""
		if len(args) == 2 {
			filter = args[1]
		}
		props, err := client.ConfigProps(ctx, args[0], filter)
		if err != nil {
			return err
		}
		if len(props) == 0 {
			return fmt.Errorf("no configuration properties matched %q on %s", filter, args[0])
		}
		return render(props)
	},
	ValidArgsFunction: completeServices,
}

var svcMappingsCmd = &cobra.Command{
	Use:   "mappings [service] [filter]",
	Short: "List the HTTP routes a running service serves",
	Long: `Lists the HTTP routes a service actually serves.

This is the authoritative endpoint inventory for a running installation — more
reliable than any documentation, because it reflects exactly what this build and
this configuration expose, including routes contributed by plugins. Pair it with
'sc api' (for Gate) or 'sc svc api' (for anything else) to call whatever it finds.`,
	Args: cobra.RangeArgs(1, 2),
	Example: `  sc svc mappings gate pipelines
  sc svc mappings orca
  sc svc mappings clouddriver manifests`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		filter := ""
		if len(args) == 2 {
			filter = args[1]
		}
		mappings, err := client.Mappings(ctx, args[0], filter)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(mappings)
		}
		t := newTable("METHODS", "PATTERN", "HANDLER")
		for _, m := range mappings {
			methods := strings.Join(m.Methods, ",")
			if methods == "" {
				methods = "*"
			}
			t.add(methods, ellipsis(m.Pattern, 56), ellipsis(shortHandler(m.Handler), 56))
		}
		t.print("No routes matched.")
		return nil
	},
	ValidArgsFunction: completeServices,
}

// shortHandler trims a Spring handler description down to Class.method, which is
// all that fits and all that is usually wanted.
func shortHandler(handler string) string {
	if handler == "" {
		return ""
	}
	// e.g. "com.netflix.spinnaker.gate.controllers.PipelineController#getPipeline(String)"
	if i := strings.Index(handler, "#"); i > 0 {
		class := handler[:i]
		method := handler[i+1:]
		if j := strings.LastIndex(class, "."); j >= 0 {
			class = class[j+1:]
		}
		if j := strings.Index(method, "("); j > 0 {
			method = method[:j]
		}
		return class + "." + method
	}
	return handler
}

var svcInfoCmd = &cobra.Command{
	Use:   "info [service]",
	Short: "Show a service's build and git metadata",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		info, err := client.Info(ctx, args[0])
		if err != nil {
			return err
		}
		if len(info) == 0 {
			fmt.Fprintf(os.Stderr, "%s reports no info detail (no build-info or git-info in the image).\n", args[0])
			return nil
		}
		return render(info)
	},
	ValidArgsFunction: completeServices,
}

var svcScheduledCmd = &cobra.Command{
	Use:     "scheduled [service]",
	Aliases: []string{"scheduledtasks"},
	Short:   "Show a service's scheduled work",
	Long: `Shows a service's scheduled work — cron, fixed-delay and fixed-rate tasks.

On echo this is how often triggers are polled; on clouddriver it is the cache
agent schedule, which is what determines how stale the infrastructure view can be.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		tasks, err := client.ScheduledTasks(ctx, args[0])
		if err != nil {
			return err
		}
		return render(tasks)
	},
	ValidArgsFunction: completeServices,
}

var svcBeansCmd = &cobra.Command{
	Use:   "beans [service] [filter]",
	Short: "List a service's Spring beans",
	Long:  "Lists a service's Spring bean names. Mostly useful to confirm whether an optional component or plugin actually loaded.",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		filter := ""
		if len(args) == 2 {
			filter = args[1]
		}
		beans, err := client.Beans(ctx, args[0], filter)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(beans)
		}
		for _, b := range beans {
			fmt.Println(b)
		}
		return nil
	},
	ValidArgsFunction: completeServices,
}

// ---------------------------------------------------------------------------
// sc svc api — the operator-plane escape hatch
// ---------------------------------------------------------------------------

var (
	svcAPIBody  string
	svcAPIQuery []string
	svcAPIRaw   bool
)

var svcAPICmd = &cobra.Command{
	Use:   "api [service] [METHOD] [PATH]",
	Short: "Call any endpoint on any service, bypassing Gate",
	Long: `Calls an arbitrary endpoint on an individual service, bypassing Gate entirely.

Gate proxies a curated subset of what the services expose. Internal endpoints —
clouddriver's cache introspection and on-demand refresh, orca's raw execution
documents and queue state, front50's unfiltered collections — have no Gate route
at all. This reaches them.

Two caveats. These endpoints are internal, so they are not a stable contract and
can change between releases. And they enforce no Fiat authorization of their own in
most cases: calling them bypasses the permission checks Gate would have applied.

'sc svc mappings <service>' lists what a given service actually serves.`,
	Args: cobra.RangeArgs(2, 3),
	Example: `  sc svc api orca GET /pipelines/01M3Y...
  sc svc api clouddriver GET /cache/introspection
  sc svc api clouddriver POST /cache/kubernetes/manifest --body '{"account":"managing","location":"default","name":"deployment nginx"}'
  sc svc api front50 GET /pipelines
  sc svc api igor GET /masters`,
	RunE: func(cmd *cobra.Command, args []string) error {
		service := args[0]
		method, path := "GET", args[1]
		if len(args) == 3 {
			method, path = strings.ToUpper(args[1]), args[2]
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		q := url.Values{}
		for _, kv := range svcAPIQuery {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("invalid --query %q: expected KEY=VALUE", kv)
			}
			q.Add(k, v)
		}
		var body []byte
		if svcAPIBody != "" {
			if strings.HasPrefix(svcAPIBody, "@") {
				raw, err := readInput(strings.TrimPrefix(svcAPIBody, "@"))
				if err != nil {
					return err
				}
				body = raw
			} else {
				body = []byte(svcAPIBody)
			}
		}

		client, err := getSvc()
		if err != nil {
			return err
		}
		if isDryRun() && method != "GET" {
			dryRunMsg("would call %s %s on %s", method, path, service)
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		raw, err := client.Do(ctx, scsvc.Request{Service: service, Method: method, Path: path, Query: q, Body: body})
		if err != nil {
			return err
		}
		if method != "GET" {
			audit("svc.api."+strings.ToLower(method), service+path)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			fmt.Fprintln(os.Stderr, "(empty response)")
			return nil
		}
		if svcAPIRaw {
			_, err := os.Stdout.Write(append(raw, '\n'))
			return err
		}
		_, err = os.Stdout.Write(prettyJSON(raw))
		return err
	},
	ValidArgsFunction: completeServices,
}

// ---------------------------------------------------------------------------
// Kubernetes-level control
// ---------------------------------------------------------------------------

var svcLogsCmd = &cobra.Command{
	Use:   "logs [service]",
	Short: "Stream a service's container logs",
	Long: `Streams a Spinnaker service's container logs from Kubernetes.

Pair this with 'sc svc set-level' — turn a category up, reproduce, and read the
result here — which together replace the redeploy such an investigation would
otherwise need.`,
	Args: cobra.ExactArgs(1),
	Example: `  sc svc logs orca --follow
  sc svc logs clouddriver --tail 200
  sc svc logs orca --previous      # the container that crashed`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		kube, err := client.Kube()
		if err != nil {
			return err
		}
		service, err := scsvc.Lookup(args[0])
		if err != nil {
			return err
		}
		// --follow has no useful deadline, so it bypasses the global timeout.
		ctx := cmd.Context()
		if !svcLogsFollow {
			var cancel func()
			ctx, cancel = cmdContext(ctx)
			defer cancel()
		}

		pods, err := kube.ListPods(ctx, client.Namespace(), "app.kubernetes.io/name="+service.Name)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return fmt.Errorf("no pods for service %s in namespace %s", service.Name, client.Namespace())
		}
		pod := pods[0].Name
		if len(pods) > 1 {
			fmt.Fprintf(os.Stderr, "%d pods for %s; streaming %s\n", len(pods), service.Name, pod)
		}

		stream, err := kube.PodLogs(ctx, client.Namespace(), pod, k8s.LogOptions{
			Follow:     svcLogsFollow,
			TailLines:  svcLogsTail,
			Previous:   svcLogsPrevious,
			Timestamps: svcLogsTimestamps,
		})
		if err != nil {
			return err
		}
		defer stream.Close()
		_, err = io.Copy(os.Stdout, stream)
		// A canceled follow is the user pressing Ctrl-C, not a failure.
		if err != nil && ctx.Err() != nil {
			return nil
		}
		return err
	},
	ValidArgsFunction: completeServices,
}

var (
	svcLogsFollow     bool
	svcLogsTail       int
	svcLogsPrevious   bool
	svcLogsTimestamps bool
)

var svcScaleCmd = &cobra.Command{
	Use:   "scale [service] [replicas]",
	Short: "Scale a service's Deployment",
	Long: `Scales a Spinnaker service's Deployment.

Scaling to 0 is the supported way to stop a service without removing it, which is
how an optional service (fiat, kayenta, keel) is disabled, and how clouddriver is
quiesced before a cache rebuild.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var replicas int
		if _, err := fmt.Sscanf(args[1], "%d", &replicas); err != nil || replicas < 0 {
			return fmt.Errorf("invalid replica count %q", args[1])
		}
		service, err := scsvc.Lookup(args[0])
		if err != nil {
			return err
		}
		client, err := getSvc()
		if err != nil {
			return err
		}
		kube, err := client.Kube()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would scale %s to %d replicas in namespace %s", service.Name, replicas, client.Namespace())
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := kube.ScaleDeployment(ctx, client.Namespace(), service.Name, replicas); err != nil {
			return err
		}
		audit("svc.scale", fmt.Sprintf("%s=%d", service.Name, replicas))
		fmt.Fprintf(os.Stderr, "Scaled %s to %d replicas\n", service.Name, replicas)
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcRestartForce bool

var svcRestartCmd = &cobra.Command{
	Use:   "restart [service]",
	Short: "Roll a service's pods",
	Long: `Triggers a rolling restart of a service's Deployment, the same way
'kubectl rollout restart' does.

Needed after a ConfigMap change, since Spinnaker services read configuration only
at startup and Kubernetes does not restart pods when a mounted ConfigMap changes.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		service, err := scsvc.Lookup(args[0])
		if err != nil {
			return err
		}
		client, err := getSvc()
		if err != nil {
			return err
		}
		kube, err := client.Kube()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would restart %s in namespace %s", service.Name, client.Namespace())
			return nil
		}
		if !svcRestartForce {
			if err := confirm(fmt.Sprintf("Roll %s pods in namespace %s?", service.Name, client.Namespace())); err != nil {
				return err
			}
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := kube.RestartDeployment(ctx, client.Namespace(), service.Name); err != nil {
			return err
		}
		audit("svc.restart", service.Name)
		fmt.Fprintf(os.Stderr, "Rolling restart triggered for %s\n", service.Name)
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcPodsCmd = &cobra.Command{
	Use:   "pods [service]",
	Short: "List the pods behind the services",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		kube, err := client.Kube()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		selector := "app.kubernetes.io/part-of=spinnaker"
		if len(args) == 1 {
			service, err := scsvc.Lookup(args[0])
			if err != nil {
				return err
			}
			selector = "app.kubernetes.io/name=" + service.Name
		}
		pods, err := kube.ListPods(ctx, client.Namespace(), selector)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(pods)
		}
		sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
		t := newTable("POD", "PHASE", "READY", "RESTARTS", "IP", "AGE")
		for _, p := range pods {
			ageStr := "-"
			if !p.StartedAt.IsZero() {
				ageStr = shortDuration(time.Since(p.StartedAt))
			}
			t.add(p.Name, p.Phase, fmt.Sprint(p.Ready), fmt.Sprint(p.Restarts), dash(p.PodIP), ageStr)
		}
		t.print("No Spinnaker pods found in namespace " + client.Namespace() + ".")
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcConfigCmd = &cobra.Command{
	Use:   "config [service]",
	Short: "Print the configuration files mounted into a service",
	Long: `Prints the configuration files currently mounted into a service, read from its
ConfigMaps.

This is the desired configuration as the cluster holds it. It is not necessarily
what the JVM resolved — for that use 'sc svc env', which reports Spring's merged
view including defaults, environment variables and active profiles. Comparing the
two is how a config change that did not take effect gets found.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getSvc()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		files, err := client.LiveConfig(ctx, args[0])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(files)
		}
		for _, name := range sortedKeys(files) {
			fmt.Printf("# ---- %s ----\n%s\n", name, strings.TrimRight(files[name], "\n"))
		}
		return nil
	},
	ValidArgsFunction: completeServices,
}

var svcActuatorConfigCmd = &cobra.Command{
	Use:   "actuator-config",
	Short: "Print the configuration that enables the operator plane",
	Long: `Prints the Spring configuration that exposes the actuator endpoints this
command group needs.

kork mounts actuator at the service root rather than /actuator, and Spring Boot
exposes only /health by default, so 'sc svc env', 'loggers', 'metrics', 'threads',
'configprops', 'mappings' and 'beans' all return 404 until these endpoints are
enabled.

Save the output as spinnaker-local.yml to apply it to every JVM service at once,
then roll the services ('sc svc restart <name>') — Spinnaker reads configuration
only at startup.`,
	Example: `  sc svc actuator-config > overlays/config/files/spinnaker-local.yml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Print(scsvc.ActuatorConfigSnippet)
		return nil
	},
}

// completeServices completes Spinnaker service names.
func completeServices(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	out := []string{}
	for _, n := range scsvc.Names() {
		if strings.HasPrefix(n, toComplete) {
			out = append(out, n)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func init() {
	svcEnvCmd.Flags().StringVar(&svcEnvFilter, "filter", "", "filter property names by substring")
	svcEnvCmd.Flags().BoolVar(&svcEnvAll, "all", false, "show every property (large)")
	svcEnvCmd.Flags().BoolVar(&svcEnvProfiles, "profiles", false, "show only the active Spring profiles")

	svcLoggersCmd.Flags().BoolVar(&svcLoggersConfigured, "configured", false, "only categories with an explicitly configured level")

	svcMetricsCmd.Flags().StringArrayVar(&svcMetricTags, "tag", nil, "filter a metric by tag KEY:VALUE (repeatable)")

	svcThreadsCmd.Flags().IntVar(&svcThreadsFrames, "frames", 5, "stack frames to show per blocked thread")
	svcThreadsCmd.Flags().BoolVar(&svcThreadsRaw, "raw", false, "print the full thread dump instead of a summary")

	svcAPICmd.Flags().StringVar(&svcAPIBody, "body", "", "request body; @file reads from a file, @- from stdin")
	svcAPICmd.Flags().StringArrayVar(&svcAPIQuery, "query", nil, "query parameter KEY=VALUE (repeatable)")
	svcAPICmd.Flags().BoolVar(&svcAPIRaw, "raw", false, "print the response body verbatim")

	svcLogsCmd.Flags().BoolVarP(&svcLogsFollow, "follow", "f", false, "stream new log lines")
	svcLogsCmd.Flags().IntVar(&svcLogsTail, "tail", 200, "lines of history to show (0 for all)")
	svcLogsCmd.Flags().BoolVar(&svcLogsPrevious, "previous", false, "logs from the previous container instance")
	svcLogsCmd.Flags().BoolVar(&svcLogsTimestamps, "timestamps", false, "prefix each line with its timestamp")

	svcRestartCmd.Flags().BoolVar(&svcRestartForce, "force", false, "skip the confirmation prompt")

	svcCmd.AddCommand(
		svcListCmd, svcHealthCmd, svcEnvCmd, svcLoggersCmd, svcSetLevelCmd, svcMetricsCmd,
		svcThreadsCmd, svcConfigPropsCmd, svcMappingsCmd, svcInfoCmd, svcScheduledCmd, svcBeansCmd,
		svcAPICmd, svcLogsCmd, svcScaleCmd, svcRestartCmd, svcPodsCmd, svcConfigCmd, svcActuatorConfigCmd,
	)
	rootCmd.AddCommand(svcCmd)
}
