package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestServerInitializeListCall(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"missing"}}`,
	}, "\n") + "\n")

	var out bytes.Buffer
	s := NewServer("jc-test", "1.0", in, &out)
	s.AddTool(Tool{
		Name:        "echo",
		Description: "echo back the msg argument",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{"echo": StringArg(args, "msg")}, nil
		},
	})

	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	// 4 requests with ids -> 4 responses; the notification yields none.
	if len(lines) != 4 {
		t.Fatalf("expected 4 responses, got %d:\n%s", len(lines), out.String())
	}

	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("initialize decode: %v", err)
	}
	if initResp.Result.ProtocolVersion != ProtocolVersion {
		t.Fatalf("bad protocol version: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"echo"`) {
		t.Fatalf("tools/list missing echo tool: %s", lines[1])
	}
	if !strings.Contains(lines[2], "hi") || !strings.Contains(lines[2], `"isError"`) {
		t.Fatalf("echo call result unexpected: %s", lines[2])
	}
	if !strings.Contains(lines[3], "unknown tool") {
		t.Fatalf("expected unknown-tool error: %s", lines[3])
	}
}

func serveLines(t *testing.T, configure func(*Server), reqs ...string) []string {
	t.Helper()
	in := strings.NewReader(strings.Join(reqs, "\n") + "\n")
	var out bytes.Buffer
	s := NewServer("t", "1", in, &out)
	configure(s)
	if err := s.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestReadOnlyModeHidesAndBlocksWrites(t *testing.T) {
	configure := func(s *Server) {
		s.SetAllowWrite(false)
		s.AddTool(Tool{Name: "r", ReadOnly: true, Handler: func(ctx context.Context, a map[string]any) (any, error) { return "ok", nil }})
		s.AddTool(Tool{Name: "w", Handler: func(ctx context.Context, a map[string]any) (any, error) { return "mutated", nil }})
	}
	lines := serveLines(t, configure,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"w"}}`,
	)
	if strings.Contains(lines[0], `"w"`) {
		t.Fatalf("write tool should be hidden in read-only mode: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"r"`) {
		t.Fatalf("read tool should be listed: %s", lines[0])
	}
	if !strings.Contains(lines[1], "disabled") || !strings.Contains(lines[1], `"isError":true`) {
		t.Fatalf("write call should be blocked: %s", lines[1])
	}
}

func TestScriptToolGated(t *testing.T) {
	mk := func(allow bool) func(*Server) {
		return func(s *Server) {
			s.SetAllowScript(allow)
			s.AddTool(Tool{Name: "groovy", Script: true, Handler: func(ctx context.Context, a map[string]any) (any, error) { return "ran", nil }})
		}
	}
	off := serveLines(t, mk(false), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if strings.Contains(off[0], "groovy") {
		t.Fatalf("script tool should be hidden without --allow-script: %s", off[0])
	}
	on := serveLines(t, mk(true), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(on[0], "groovy") {
		t.Fatalf("script tool should be listed with --allow-script: %s", on[0])
	}
}

func TestStructuredToolError(t *testing.T) {
	configure := func(s *Server) {
		s.AddTool(Tool{Name: "boom", Handler: func(ctx context.Context, a map[string]any) (any, error) {
			return nil, Errorf("bad_input", "missing %s", "x")
		}})
	}
	lines := serveLines(t, configure, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`)
	if !strings.Contains(lines[0], "bad_input") || !strings.Contains(lines[0], "missing x") || !strings.Contains(lines[0], `"isError":true`) {
		t.Fatalf("expected structured error: %s", lines[0])
	}
}

func TestAnnotationsInToolList(t *testing.T) {
	configure := func(s *Server) {
		s.AddTool(Tool{Name: "del", Destructive: true, Handler: func(ctx context.Context, a map[string]any) (any, error) { return nil, nil }})
	}
	lines := serveLines(t, configure, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(lines[0], `"destructiveHint":true`) {
		t.Fatalf("expected destructive annotation: %s", lines[0])
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	configure := func(s *Server) {
		s.AddResourceTemplate(ResourceTemplate{URITemplate: "x://{n}", Name: "x"})
		s.SetResourceLister(func(ctx context.Context) ([]Resource, error) {
			return []Resource{{URI: "x://1", Name: "one"}}, nil
		})
		s.SetResourceReader(func(ctx context.Context, uri string) (ResourceContent, error) {
			if uri != "x://1" {
				return ResourceContent{}, errors.New("not found")
			}
			return ResourceContent{URI: uri, MimeType: "text/plain", Text: "hello"}, nil
		})
		s.AddPrompt(Prompt{
			Name: "greet", Description: "greet",
			Arguments: []PromptArg{{Name: "who", Required: true}},
			Handler: func(ctx context.Context, args map[string]string) (PromptResult, error) {
				return PromptResult{Description: "g", Messages: []PromptMessage{{Role: "user", Text: "hi " + args["who"]}}}, nil
			},
		})
	}
	lines := serveLines(t, configure,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/templates/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"x://1"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"prompts/list"}`,
		`{"jsonrpc":"2.0","id":6,"method":"prompts/get","params":{"name":"greet","arguments":{"who":"bob"}}}`,
	)
	if len(lines) != 6 {
		t.Fatalf("expected 6 responses, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], `"resources"`) || !strings.Contains(lines[0], `"prompts"`) {
		t.Fatalf("initialize should advertise resources+prompts caps: %s", lines[0])
	}
	if !strings.Contains(lines[1], "x://{n}") {
		t.Fatalf("templates/list: %s", lines[1])
	}
	if !strings.Contains(lines[2], "x://1") {
		t.Fatalf("resources/list: %s", lines[2])
	}
	if !strings.Contains(lines[3], "hello") {
		t.Fatalf("resources/read: %s", lines[3])
	}
	if !strings.Contains(lines[4], "greet") {
		t.Fatalf("prompts/list: %s", lines[4])
	}
	if !strings.Contains(lines[5], "hi bob") {
		t.Fatalf("prompts/get: %s", lines[5])
	}
}
