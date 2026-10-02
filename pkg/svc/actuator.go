package svc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ActuatorConfigSnippet is the configuration that exposes the operator-plane
// endpoints. kork's SpringBoot1CompatibilityApplicationListener sets
// management.endpoints.web.base-path to "/" for /health backwards compatibility,
// so these land at the service root — /env, /loggers and so on, not /actuator/*.
//
// Spring Boot exposes only /health by default; everything else 404s until listed
// here. Mount this as spinnaker-local.yml to apply it to every JVM service at
// once (Spinnaker services load both spinnaker.yml and <service>.yml, plus their
// -local profile variants).
const ActuatorConfigSnippet = `# spinnaker-local.yml — applies to every Spinnaker JVM service.
#
# LAB / TRUSTED NETWORKS ONLY.
#   /env and /configprops print resolved configuration values, which include
#   credentials. /heapdump writes a full heap image. Expose these only where the
#   service port is not reachable by untrusted callers, and prefer dropping
#   env/configprops/heapdump in shared environments.
management:
  endpoints:
    web:
      exposure:
        include: >-
          health,info,env,loggers,metrics,threaddump,heapdump,configprops,
          mappings,beans,caches,scheduledtasks,conditions,httpexchanges,quartz,liquibase
  endpoint:
    health:
      show-details: always
      show-components: always
    env:
      show-values: always
    configprops:
      show-values: always
  info:
    env:
      enabled: true
    build:
      enabled: true
    git:
      mode: full
`

// Health returns a service's actuator health document. With
// management.endpoint.health.show-details=always this includes per-component
// status (redis, db, sql, downstream services), which is what makes it useful
// for diagnosis rather than just liveness.
func (c *Client) Health(ctx context.Context, service string) (map[string]any, error) {
	var out map[string]any
	return out, c.GetJSON(ctx, service, "/health", nil, &out)
}

// Info returns a service's actuator info document (build and git metadata).
func (c *Client) Info(ctx context.Context, service string) (map[string]any, error) {
	var out map[string]any
	return out, c.GetJSON(ctx, service, "/info", nil, &out)
}

// EnvProperty is one resolved Spring property and where it came from.
type EnvProperty struct {
	Name   string `json:"name" yaml:"name"`
	Value  string `json:"value" yaml:"value"`
	Source string `json:"source" yaml:"source"`
	// Origin is the exact file and line Spring attributes the value to, when known.
	Origin string `json:"origin,omitempty" yaml:"origin,omitempty"`
	// Overridden marks a value that a higher-precedence source shadows. This is
	// the single most useful field when a config change appears to have no effect.
	Overridden bool `json:"overridden,omitempty" yaml:"overridden,omitempty"`
}

// Env returns a service's resolved Spring environment.
//
// Properties are returned in precedence order with shadowed duplicates marked,
// which answers the question operators actually have: not "what is in the config
// file" but "what value is this service using, and which file won".
//
// pattern, when non-empty, filters property names by substring (case-insensitive).
func (c *Client) Env(ctx context.Context, service, pattern string) ([]EnvProperty, error) {
	var env struct {
		ActiveProfiles  []string `json:"activeProfiles"`
		PropertySources []struct {
			Name       string `json:"name"`
			Properties map[string]struct {
				Value  any    `json:"value"`
				Origin string `json:"origin"`
			} `json:"properties"`
		} `json:"propertySources"`
	}
	if err := c.GetJSON(ctx, service, "/env", nil, &env); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	out := []EnvProperty{}
	needle := strings.ToLower(pattern)
	for _, ps := range env.PropertySources {
		names := make([]string, 0, len(ps.Properties))
		for name := range ps.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if needle != "" && !strings.Contains(strings.ToLower(name), needle) {
				continue
			}
			p := ps.Properties[name]
			out = append(out, EnvProperty{
				Name:       name,
				Value:      fmt.Sprint(p.Value),
				Source:     ps.Name,
				Origin:     p.Origin,
				Overridden: seen[name],
			})
			seen[name] = true
		}
	}
	return out, nil
}

// ActiveProfiles returns a service's active Spring profiles. Spinnaker uses
// profiles for environment overlays, so a missing profile is a common cause of
// "my config was ignored".
func (c *Client) ActiveProfiles(ctx context.Context, service string) ([]string, error) {
	var env struct {
		ActiveProfiles []string `json:"activeProfiles"`
	}
	if err := c.GetJSON(ctx, service, "/env", nil, &env); err != nil {
		return nil, err
	}
	return env.ActiveProfiles, nil
}

// Logger is one log category's configured and effective level.
type Logger struct {
	Name            string `json:"name" yaml:"name"`
	ConfiguredLevel string `json:"configuredLevel,omitempty" yaml:"configuredLevel,omitempty"`
	EffectiveLevel  string `json:"effectiveLevel" yaml:"effectiveLevel"`
}

// Loggers returns a service's log categories.
//
// pattern filters by substring. Without a filter this returns thousands of
// categories, so callers should almost always pass one.
// onlyConfigured keeps just the categories with an explicit level set — i.e.
// the ones someone changed.
func (c *Client) Loggers(ctx context.Context, service, pattern string, onlyConfigured bool) ([]Logger, error) {
	var resp struct {
		Levels  []string `json:"levels"`
		Loggers map[string]struct {
			ConfiguredLevel string `json:"configuredLevel"`
			EffectiveLevel  string `json:"effectiveLevel"`
		} `json:"loggers"`
	}
	if err := c.GetJSON(ctx, service, "/loggers", nil, &resp); err != nil {
		return nil, err
	}
	needle := strings.ToLower(pattern)
	out := []Logger{}
	for name, l := range resp.Loggers {
		if needle != "" && !strings.Contains(strings.ToLower(name), needle) {
			continue
		}
		if onlyConfigured && l.ConfiguredLevel == "" {
			continue
		}
		out = append(out, Logger{Name: name, ConfiguredLevel: l.ConfiguredLevel, EffectiveLevel: l.EffectiveLevel})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetLogger returns one log category's levels.
func (c *Client) GetLogger(ctx context.Context, service, logger string) (Logger, error) {
	var resp struct {
		ConfiguredLevel string `json:"configuredLevel"`
		EffectiveLevel  string `json:"effectiveLevel"`
	}
	if err := c.GetJSON(ctx, service, "/loggers/"+url.PathEscape(logger), nil, &resp); err != nil {
		return Logger{}, err
	}
	return Logger{Name: logger, ConfiguredLevel: resp.ConfiguredLevel, EffectiveLevel: resp.EffectiveLevel}, nil
}

// LogLevels are the levels the actuator accepts.
var LogLevels = []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL", "OFF"}

// SetLogLevel changes a log category's level on a running service, with no
// restart and no config change. Passing an empty level resets the category to
// inherit from its parent.
//
// This is the operator plane's highest-value write: it turns a reproduction that
// needs a redeploy into one that needs a single command.
func (c *Client) SetLogLevel(ctx context.Context, service, logger, level string) error {
	body := []byte("{}")
	if level != "" {
		lv := strings.ToUpper(level)
		valid := false
		for _, l := range LogLevels {
			if l == lv {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("invalid log level %q; valid levels: %s", level, strings.Join(LogLevels, ", "))
		}
		body = []byte(fmt.Sprintf(`{"configuredLevel":%q}`, lv))
	}
	_, err := c.Do(ctx, Request{
		Service: service,
		Method:  http.MethodPost,
		Path:    "/loggers/" + url.PathEscape(logger),
		Body:    body,
	})
	return err
}

// MetricNames returns the metric names a service publishes.
func (c *Client) MetricNames(ctx context.Context, service, pattern string) ([]string, error) {
	var resp struct {
		Names []string `json:"names"`
	}
	if err := c.GetJSON(ctx, service, "/metrics", nil, &resp); err != nil {
		return nil, err
	}
	if pattern == "" {
		sort.Strings(resp.Names)
		return resp.Names, nil
	}
	needle := strings.ToLower(pattern)
	out := []string{}
	for _, n := range resp.Names {
		if strings.Contains(strings.ToLower(n), needle) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Metric is one metric's measurements.
type Metric struct {
	Name         string             `json:"name" yaml:"name"`
	Description  string             `json:"description,omitempty" yaml:"description,omitempty"`
	BaseUnit     string             `json:"baseUnit,omitempty" yaml:"baseUnit,omitempty"`
	Measurements map[string]float64 `json:"measurements" yaml:"measurements"`
	// AvailableTags lists the tag keys and values this metric can be sliced by.
	AvailableTags map[string][]string `json:"availableTags,omitempty" yaml:"availableTags,omitempty"`
}

// Metric returns one metric, optionally filtered by tag ("key:value" pairs).
func (c *Client) Metric(ctx context.Context, service, name string, tags []string) (Metric, error) {
	q := url.Values{}
	for _, t := range tags {
		q.Add("tag", t)
	}
	var resp struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		BaseUnit     string `json:"baseUnit"`
		Measurements []struct {
			Statistic string `json:"statistic"`
			// Spring serialises non-finite doubles as the JSON strings "NaN",
			// "Infinity" and "-Infinity", so this cannot be a float64: an idle
			// gauge would fail the whole decode.
			Value any `json:"value"`
		} `json:"measurements"`
		AvailableTags []struct {
			Tag    string   `json:"tag"`
			Values []string `json:"values"`
		} `json:"availableTags"`
	}
	if err := c.GetJSON(ctx, service, "/metrics/"+url.PathEscape(name), q, &resp); err != nil {
		return Metric{}, err
	}
	m := Metric{
		Name:         resp.Name,
		Description:  resp.Description,
		BaseUnit:     resp.BaseUnit,
		Measurements: map[string]float64{},
	}
	for _, meas := range resp.Measurements {
		m.Measurements[meas.Statistic] = toFloat(meas.Value)
	}
	if len(resp.AvailableTags) > 0 {
		m.AvailableTags = map[string][]string{}
		for _, t := range resp.AvailableTags {
			m.AvailableTags[t.Tag] = t.Values
		}
	}
	return m, nil
}

// toFloat coerces an actuator measurement to a float, mapping Spring's
// non-finite string encodings onto their IEEE 754 values.
func toFloat(v any) float64 {
	switch typed := v.(type) {
	case float64:
		return typed
	case string:
		switch typed {
		case "NaN":
			return math.NaN()
		case "Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
		if f, err := strconv.ParseFloat(typed, 64); err == nil {
			return f
		}
	}
	return math.NaN()
}

// ThreadSummary is a condensed thread dump: counts by state plus the threads
// that are blocked or deadlocked.
type ThreadSummary struct {
	Total      int            `json:"total" yaml:"total"`
	ByState    map[string]int `json:"byState" yaml:"byState"`
	Blocked    []ThreadInfo   `json:"blocked,omitempty" yaml:"blocked,omitempty"`
	Deadlocked []ThreadInfo   `json:"deadlocked,omitempty" yaml:"deadlocked,omitempty"`
}

// ThreadInfo is one thread's identity and top frames.
type ThreadInfo struct {
	Name      string   `json:"name" yaml:"name"`
	ID        int64    `json:"id" yaml:"id"`
	State     string   `json:"state" yaml:"state"`
	LockName  string   `json:"lockName,omitempty" yaml:"lockName,omitempty"`
	LockOwner string   `json:"lockOwner,omitempty" yaml:"lockOwner,omitempty"`
	Top       []string `json:"top,omitempty" yaml:"top,omitempty"`
}

// ThreadDump returns a service's raw actuator thread dump.
func (c *Client) ThreadDump(ctx context.Context, service string) ([]byte, error) {
	return c.Do(ctx, Request{Service: service, Path: "/threaddump"})
}

// ThreadSummary condenses a thread dump into state counts and the blocked
// threads, which is what identifies a stuck orca or a clouddriver wedged on a
// provider call.
func (c *Client) ThreadSummary(ctx context.Context, service string, frames int) (ThreadSummary, error) {
	if frames <= 0 {
		frames = 5
	}
	raw, err := c.ThreadDump(ctx, service)
	if err != nil {
		return ThreadSummary{}, err
	}
	var dump struct {
		Threads []struct {
			ThreadName    string `json:"threadName"`
			ThreadID      int64  `json:"threadId"`
			ThreadState   string `json:"threadState"`
			LockName      string `json:"lockName"`
			LockOwnerName string `json:"lockOwnerName"`
			StackTrace    []struct {
				ClassName  string `json:"className"`
				MethodName string `json:"methodName"`
				FileName   string `json:"fileName"`
				LineNumber int    `json:"lineNumber"`
			} `json:"stackTrace"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		return ThreadSummary{}, fmt.Errorf("parsing thread dump: %w", err)
	}

	sum := ThreadSummary{Total: len(dump.Threads), ByState: map[string]int{}}
	for _, t := range dump.Threads {
		sum.ByState[t.ThreadState]++
		if t.ThreadState != "BLOCKED" {
			continue
		}
		info := ThreadInfo{
			Name:      t.ThreadName,
			ID:        t.ThreadID,
			State:     t.ThreadState,
			LockName:  t.LockName,
			LockOwner: t.LockOwnerName,
		}
		for i, f := range t.StackTrace {
			if i >= frames {
				break
			}
			info.Top = append(info.Top, fmt.Sprintf("%s.%s(%s:%d)", f.ClassName, f.MethodName, f.FileName, f.LineNumber))
		}
		sum.Blocked = append(sum.Blocked, info)
	}
	return sum, nil
}

// HeapDump downloads a service's heap dump. These are large (gigabytes for a
// busy clouddriver), so the caller streams it to disk rather than buffering.
func (c *Client) HeapDump(ctx context.Context, service string) ([]byte, error) {
	return c.Do(ctx, Request{Service: service, Path: "/heapdump"})
}

// ConfigProps returns a service's @ConfigurationProperties beans with their
// bound values — the typed view of configuration, as opposed to Env's flat
// property list.
func (c *Client) ConfigProps(ctx context.Context, service, pattern string) (map[string]any, error) {
	var resp struct {
		Contexts map[string]struct {
			Beans map[string]struct {
				Prefix     string         `json:"prefix"`
				Properties map[string]any `json:"properties"`
			} `json:"beans"`
		} `json:"contexts"`
	}
	if err := c.GetJSON(ctx, service, "/configprops", nil, &resp); err != nil {
		return nil, err
	}
	needle := strings.ToLower(pattern)
	out := map[string]any{}
	for _, ctxv := range resp.Contexts {
		for bean, b := range ctxv.Beans {
			key := b.Prefix
			if key == "" {
				key = bean
			}
			if needle != "" && !strings.Contains(strings.ToLower(key), needle) && !strings.Contains(strings.ToLower(bean), needle) {
				continue
			}
			out[key] = b.Properties
		}
	}
	return out, nil
}

// Mapping is one HTTP route a service serves.
type Mapping struct {
	Pattern string   `json:"pattern" yaml:"pattern"`
	Methods []string `json:"methods,omitempty" yaml:"methods,omitempty"`
	Handler string   `json:"handler,omitempty" yaml:"handler,omitempty"`
}

// Mappings returns a service's HTTP routes.
//
// This is the authoritative endpoint inventory for a running service — more
// reliable than documentation, since it reflects exactly what this build and
// this configuration expose, including plugin-contributed routes.
func (c *Client) Mappings(ctx context.Context, service, pattern string) ([]Mapping, error) {
	var resp struct {
		Contexts map[string]struct {
			Mappings struct {
				DispatcherServlets map[string][]struct {
					Predicate string `json:"predicate"`
					Handler   string `json:"handler"`
					Details   *struct {
						RequestMappingConditions *struct {
							Patterns []string `json:"patterns"`
							Methods  []string `json:"methods"`
						} `json:"requestMappingConditions"`
					} `json:"details"`
				} `json:"dispatcherServlets"`
			} `json:"mappings"`
		} `json:"contexts"`
	}
	if err := c.GetJSON(ctx, service, "/mappings", nil, &resp); err != nil {
		return nil, err
	}
	needle := strings.ToLower(pattern)
	out := []Mapping{}
	for _, cx := range resp.Contexts {
		for _, entries := range cx.Mappings.DispatcherServlets {
			for _, e := range entries {
				m := Mapping{Pattern: e.Predicate, Handler: e.Handler}
				if e.Details != nil && e.Details.RequestMappingConditions != nil {
					rc := e.Details.RequestMappingConditions
					if len(rc.Patterns) > 0 {
						m.Pattern = strings.Join(rc.Patterns, ", ")
					}
					m.Methods = rc.Methods
				}
				if needle != "" && !strings.Contains(strings.ToLower(m.Pattern), needle) && !strings.Contains(strings.ToLower(m.Handler), needle) {
					continue
				}
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out, nil
}

// Beans returns a service's Spring bean names, optionally filtered.
func (c *Client) Beans(ctx context.Context, service, pattern string) ([]string, error) {
	var resp struct {
		Contexts map[string]struct {
			Beans map[string]any `json:"beans"`
		} `json:"contexts"`
	}
	if err := c.GetJSON(ctx, service, "/beans", nil, &resp); err != nil {
		return nil, err
	}
	needle := strings.ToLower(pattern)
	out := []string{}
	for _, cx := range resp.Contexts {
		for name := range cx.Beans {
			if needle != "" && !strings.Contains(strings.ToLower(name), needle) {
				continue
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ScheduledTasks returns a service's scheduled work (cron, fixed-delay,
// fixed-rate). On echo this is how trigger polling is configured; on clouddriver
// it is the cache agent schedule.
func (c *Client) ScheduledTasks(ctx context.Context, service string) (map[string]any, error) {
	var out map[string]any
	return out, c.GetJSON(ctx, service, "/scheduledtasks", nil, &out)
}
