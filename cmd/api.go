package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
)

var (
	apiBody    string
	apiQuery   []string
	apiHeaders []string
	apiRaw     bool
	apiStatus  bool
)

var apiCmd = &cobra.Command{
	Use:   "api [METHOD] [PATH]",
	Short: "Call any Gate endpoint directly",
	Long: `Calls any Gate endpoint with the active context's credentials.

This is the coverage guarantee: Gate exposes 70-odd controllers and sc wraps the
ones worth wrapping, but anything it has not — a provider-specific endpoint, a new
upstream addition, a plugin's routes — is still one command away.

PATH is relative to the configured endpoint, so it must not repeat Gate's context
path. With --gate http://host/api/v1, use /applications, not /api/v1/applications.

To discover what a running installation actually serves, use the operator plane:
'sc svc mappings gate' lists every route this build and configuration expose.`,
	Args: cobra.RangeArgs(1, 2),
	Example: `  sc api GET /applications
  sc api GET /applications/demo/pipelineConfigs
  sc api /version                                   # GET is the default
  sc api GET /executions --query pipelineConfigIds=abc --query limit=5
  sc api POST /webhooks/webhook/my-trigger --body '{"parameters":{"branch":"main"}}'
  sc api PUT /pipelines/01M3Y.../cancel
  sc api GET /applications --raw | jq '.[].name'`,
	GroupID: GroupCore,
	RunE: func(cmd *cobra.Command, args []string) error {
		method, path := "GET", args[0]
		if len(args) == 2 {
			method, path = strings.ToUpper(args[0]), args[1]
		} else if isHTTPMethod(args[0]) {
			return fmt.Errorf("a path is required: sc api %s /some/path", strings.ToUpper(args[0]))
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		q := url.Values{}
		for _, kv := range apiQuery {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("invalid --query %q: expected KEY=VALUE", kv)
			}
			q.Add(k, v)
		}

		var body any
		if apiBody != "" {
			// A leading @ reads the body from a file, as curl does.
			if strings.HasPrefix(apiBody, "@") {
				raw, err := readInput(strings.TrimPrefix(apiBody, "@"))
				if err != nil {
					return err
				}
				body = raw
			} else {
				body = []byte(apiBody)
			}
		}

		headers := map[string]string{}
		for _, h := range apiHeaders {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				return fmt.Errorf("invalid --header %q: expected 'Name: value'", h)
			}
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}

		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() && method != "GET" {
			dryRunMsg("would call %s %s", method, path)
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		status, raw, err := client.Raw(ctx, gate.Request{
			Method: method, Path: path, Query: q, Body: body, Headers: headers,
		})
		if apiStatus {
			fmt.Fprintf(os.Stderr, "HTTP %d\n", status)
		}
		if err != nil {
			return err
		}
		if method != "GET" {
			audit("api."+strings.ToLower(method), path)
		}

		if len(strings.TrimSpace(string(raw))) == 0 {
			fmt.Fprintf(os.Stderr, "HTTP %d (empty response)\n", status)
			return nil
		}
		if apiRaw {
			_, err := os.Stdout.Write(append(raw, '\n'))
			return err
		}
		_, err = os.Stdout.Write(prettyJSON(raw))
		return err
	},
}

func isHTTPMethod(s string) bool {
	switch strings.ToUpper(s) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	}
	return false
}

func init() {
	apiCmd.Flags().StringVar(&apiBody, "body", "", "request body; @file reads from a file, @- from stdin")
	apiCmd.Flags().StringArrayVar(&apiQuery, "query", nil, "query parameter KEY=VALUE (repeatable)")
	apiCmd.Flags().StringArrayVarP(&apiHeaders, "header", "H", nil, "extra request header 'Name: value' (repeatable)")
	apiCmd.Flags().BoolVar(&apiRaw, "raw", false, "print the response body verbatim instead of re-indenting it")
	apiCmd.Flags().BoolVar(&apiStatus, "status", false, "print the HTTP status to stderr")
	rootCmd.AddCommand(apiCmd)
}
