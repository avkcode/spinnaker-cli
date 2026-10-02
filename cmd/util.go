package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// table is a small builder so each command renders a table the same way.
type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table { return &table{headers: headers} }

func (t *table) add(cells ...string) {
	t.rows = append(t.rows, cells)
}

// print writes the table, or a short note to stderr when there are no rows so an
// empty result is never mistaken for a broken command.
func (t *table) print(emptyNote string) {
	w := getOutput()
	if len(t.rows) == 0 {
		if emptyNote != "" {
			fmt.Fprintln(os.Stderr, emptyNote)
		}
		return
	}
	tw := w.Table()
	fmt.Fprintln(tw, strings.Join(t.headers, "\t"))
	for _, r := range t.rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	w.FlushTable(tw)
}

// ---------------------------------------------------------------------------
// field extraction
//
// Gate hands back a downstream service's model largely untyped, and different
// providers populate different fields. These helpers read a value if it is there
// and yield an empty string if it is not, rather than forcing every command to
// repeat the same type switches.
// ---------------------------------------------------------------------------

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case bool:
		return strconv.FormatBool(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// strOr returns the first key that has a non-empty value.
func strOr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v := str(m, k); v != "" {
			return v
		}
	}
	return ""
}

func num(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func boolean(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

func mapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func listField(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	if v, ok := m[key].([]any); ok {
		return v
	}
	return nil
}

// mapList coerces a []any of objects into []map[string]any, skipping non-objects.
func mapList(in []any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, v := range in {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// formatting
// ---------------------------------------------------------------------------

// epochTime renders a Spinnaker millisecond timestamp. Spinnaker stores every
// time as epoch millis, including as a JSON string in some front50 models.
func epochTime(m map[string]any, key string) string {
	ms, ok := num(m, key)
	if !ok || ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

// age renders a millisecond timestamp as an elapsed duration.
func age(m map[string]any, key string) string {
	ms, ok := num(m, key)
	if !ok || ms <= 0 {
		return "-"
	}
	return shortDuration(time.Since(time.UnixMilli(ms)))
}

// durationMillis renders a millisecond duration.
func durationMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return shortDuration(time.Duration(ms) * time.Millisecond)
}

// execDuration computes a stage or execution duration from its start/end times,
// treating a missing endTime as "still running".
func execDuration(m map[string]any) string {
	start, ok := num(m, "startTime")
	if !ok || start <= 0 {
		return "-"
	}
	end, ok := num(m, "endTime")
	if !ok || end <= 0 {
		return shortDuration(time.Since(time.UnixMilli(start))) + "+"
	}
	return durationMillis(end - start)
}

// shortDuration renders a duration compactly: 3d4h, 2h5m, 90ms.
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// ellipsis truncates to n runes, marking the cut.
func ellipsis(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if n <= 1 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// dash renders an empty string as "-" so table columns stay aligned.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// ---------------------------------------------------------------------------
// input
// ---------------------------------------------------------------------------

// readInput reads a file, or stdin when path is "-" or empty.
func readInput(path string) ([]byte, error) {
	if path == "" || path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// readJSONOrYAML reads JSON or YAML from a file or stdin into a map. Pipeline
// definitions are JSON in Spinnaker, but accepting YAML makes them far easier to
// keep in version control.
func readJSONOrYAML(path string) (map[string]any, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("no input: %s is empty", describeInput(path))
	}
	out, err := readJSONOrYAMLBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", describeInput(path), err)
	}
	return out, nil
}

// readJSONOrYAMLBytes decodes an object from JSON or YAML bytes.
//
// YAML is a superset of JSON, so one parser would handle both; JSON is tried
// first because its errors point at the real problem when the input is
// JSON-shaped but malformed.
func readJSONOrYAMLBytes(raw []byte) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err == nil {
		return out, nil
	}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("input is neither a JSON nor a YAML object: %w", err)
	}
	return out, nil
}

// readJSONOrYAMLList reads a JSON/YAML array of objects.
func readJSONOrYAMLList(path string) ([]map[string]any, error) {
	raw, err := readInput(path)
	if err != nil {
		return nil, err
	}
	out, err := readJSONOrYAMLListBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", describeInput(path), err)
	}
	return out, nil
}

// readJSONOrYAMLListBytes decodes an array of objects from JSON or YAML bytes.
func readJSONOrYAMLListBytes(raw []byte) ([]map[string]any, error) {
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err == nil {
		return out, nil
	}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("input is neither a JSON nor a YAML list of objects: %w", err)
	}
	return out, nil
}

func describeInput(path string) string {
	if path == "" || path == "-" {
		return "stdin"
	}
	return path
}

// parseKeyValues turns KEY=VALUE flags into a map. Values may contain "=".
func parseKeyValues(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid parameter %q: expected KEY=VALUE", p)
		}
		out[k] = v
	}
	return out, nil
}

// sortedKeys returns a map's keys in order, for stable output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeOut writes raw bytes to a file, or stdout when path is empty or "-".
func writeOut(path string, data []byte) error {
	if path == "" || path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// prettyJSON re-indents JSON for human-readable output.
func prettyJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return append(out, '\n')
}
