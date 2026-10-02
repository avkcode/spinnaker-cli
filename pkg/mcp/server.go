// Package mcp implements a minimal Model Context Protocol (MCP) server over a
// newline-delimited JSON-RPC 2.0 stdio transport, with a simple tool registry.
// It lets agents (e.g. opencode, Claude Code) drive sc operations natively, with
// every tool returning JSON-structured output.
//
// This is deliberately a stdio server rather than a client of Gate's own
// embedded MCP endpoint (gate-mcp): it exposes the operator/platform-plane
// capabilities that live in the CLI (actuator access, Kubernetes-level service
// control, live-vs-desired config diffing) and works against installations
// where mcp.server.enabled is false, which is the default.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolVersion is the MCP protocol version advertised by this server.
const ProtocolVersion = "2024-11-05"

// ToolHandler executes a tool with decoded arguments and returns a JSON-able result.
type ToolHandler func(ctx context.Context, args map[string]any) (any, error)

// Tool is a registered MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     ToolHandler

	// Classification, surfaced as MCP annotations and used for gating.
	ReadOnly    bool // does not mutate controller state
	Destructive bool // may delete/disrupt (cancel, delete, restart, scale)
	Idempotent  bool // repeating the call has the same effect
	Script      bool // reaches past the Gate API into service internals or the cluster; gated behind --allow-script
}

// ToolError is a structured tool failure surfaced to the client as a parseable
// {code, message} payload rather than an opaque string.
type ToolError struct {
	Code    string
	Message string
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// Errorf builds a *ToolError with a formatted message.
func Errorf(code, format string, a ...any) *ToolError {
	return &ToolError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Resource is a readable MCP resource (e.g. a job config or build log).
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceTemplate advertises a parameterized resource URI (RFC 6570 style).
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ResourceContent is the body returned by a resources/read.
type ResourceContent struct {
	URI      string
	MimeType string
	Text     string
}

// ResourceLister returns the concrete resources available right now.
type ResourceLister func(ctx context.Context) ([]Resource, error)

// ResourceReader resolves a resource URI to its content.
type ResourceReader func(ctx context.Context, uri string) (ResourceContent, error)

// PromptArg describes a prompt argument.
type PromptArg struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is one message in a rendered prompt.
type PromptMessage struct {
	Role string // "user" or "assistant"
	Text string
}

// PromptResult is the rendered prompt returned by prompts/get.
type PromptResult struct {
	Description string
	Messages    []PromptMessage
}

// PromptHandler renders a prompt from its (string) arguments.
type PromptHandler func(ctx context.Context, args map[string]string) (PromptResult, error)

// Prompt is a registered MCP prompt template.
type Prompt struct {
	Name        string
	Description string
	Arguments   []PromptArg
	Handler     PromptHandler
}

// Server is an MCP server bound to an input/output stream.
type Server struct {
	name        string
	version     string
	tools       map[string]Tool
	order       []string
	in          io.Reader
	out         io.Writer
	allowWrite  bool
	allowScript bool

	resourceTemplates []ResourceTemplate
	resourceLister    ResourceLister
	resourceReader    ResourceReader
	prompts           map[string]Prompt
	promptOrder       []string
}

// NewServer creates a server reading requests from in and writing responses to
// out. By default mutating tools are allowed but service-internal tools (direct
// microservice calls, actuator writes, cluster mutations) are not.
func NewServer(name, version string, in io.Reader, out io.Writer) *Server {
	return &Server{name: name, version: version, tools: map[string]Tool{}, in: in, out: out, allowWrite: true}
}

// SetAllowWrite enables or disables mutating tools (false = read-only mode).
func (s *Server) SetAllowWrite(b bool) { s.allowWrite = b }

// SetAllowScript enables or disables service-internal tools (direct microservice
// calls that bypass Gate, actuator writes, and cluster-level mutations).
func (s *Server) SetAllowScript(b bool) { s.allowScript = b }

// toolAllowed reports whether a tool may be listed/called under the current mode.
func (s *Server) toolAllowed(t Tool) bool {
	if t.Script && !s.allowScript {
		return false
	}
	if !t.ReadOnly && !s.allowWrite {
		return false
	}
	return true
}

// SetResourceLister registers the dynamic resource listing callback.
func (s *Server) SetResourceLister(l ResourceLister) { s.resourceLister = l }

// SetResourceReader registers the resource read callback (enables resources).
func (s *Server) SetResourceReader(r ResourceReader) { s.resourceReader = r }

// AddResourceTemplate advertises a parameterized resource URI.
func (s *Server) AddResourceTemplate(t ResourceTemplate) {
	s.resourceTemplates = append(s.resourceTemplates, t)
}

// AddPrompt registers a prompt template.
func (s *Server) AddPrompt(p Prompt) {
	if s.prompts == nil {
		s.prompts = map[string]Prompt{}
	}
	if _, exists := s.prompts[p.Name]; !exists {
		s.promptOrder = append(s.promptOrder, p.Name)
	}
	s.prompts[p.Name] = p
}

func (s *Server) hasResources() bool { return s.resourceReader != nil }
func (s *Server) hasPrompts() bool   { return len(s.prompts) > 0 }

// AddTool registers a tool (last registration of a name wins; order preserved).
func (s *Server) AddTool(t Tool) {
	if _, exists := s.tools[t.Name]; !exists {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
}

// ToolNames returns all registered tool names in registration order (regardless
// of read-only/script gating). Useful for parity tests.
func (s *Server) ToolNames() []string {
	names := make([]string, len(s.order))
	copy(names, s.order)
	return names
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads JSON-RPC messages until EOF, dispatching each.
func (s *Server) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(s.out)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		resp, respond := s.handle(ctx, &req)
		if respond {
			if err := enc.Encode(resp); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func (s *Server) handle(ctx context.Context, req *rpcRequest) (rpcResponse, bool) {
	// Requests without an id are notifications: handle silently, no response.
	if len(req.ID) == 0 {
		return rpcResponse{}, false
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		caps := map[string]any{"tools": map[string]any{}}
		if s.hasResources() {
			caps["resources"] = map[string]any{}
		}
		if s.hasPrompts() {
			caps["prompts"] = map[string]any{}
		}
		resp.Result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    caps,
			"serverInfo":      map[string]any{"name": s.name, "version": s.version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.toolList()}
	case "tools/call":
		resp.Result, resp.Error = s.callTool(ctx, req.Params)
	case "resources/list":
		resp.Result, resp.Error = s.listResources(ctx)
	case "resources/templates/list":
		templates := s.resourceTemplates
		if templates == nil {
			templates = []ResourceTemplate{}
		}
		resp.Result = map[string]any{"resourceTemplates": templates}
	case "resources/read":
		resp.Result, resp.Error = s.readResource(ctx, req.Params)
	case "prompts/list":
		resp.Result = map[string]any{"prompts": s.promptList()}
	case "prompts/get":
		resp.Result, resp.Error = s.getPrompt(ctx, req.Params)
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, true
}

func (s *Server) toolList() []map[string]any {
	list := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		if !s.toolAllowed(t) {
			continue
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		list = append(list, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": schema,
			"annotations": map[string]any{
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"idempotentHint":  t.Idempotent,
			},
		})
	}
	return list
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
	if !s.toolAllowed(tool) {
		reason := "tool is disabled in read-only mode"
		if tool.Script {
			reason = "service-internal tools are disabled; start the server with --allow-script"
		}
		return toolErrorResult("disabled", reason), nil
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	result, err := tool.Handler(ctx, p.Arguments)
	if err != nil {
		var te *ToolError
		if errors.As(err, &te) {
			return toolErrorResult(te.Code, te.Message), nil
		}
		return toolErrorResult("error", err.Error()), nil
	}
	js, mErr := json.MarshalIndent(result, "", "  ")
	if mErr != nil {
		return toolErrorResult("encode_error", mErr.Error()), nil
	}
	return toolResult(string(js), false), nil
}

// toolErrorResult returns a tool result whose content is a parseable
// {code, message} JSON object, with isError set.
func toolErrorResult(code, message string) map[string]any {
	payload, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(payload)}},
		"isError": true,
	}
}

// BoolArg returns a bool argument by name (false if missing/not a bool).
func BoolArg(args map[string]any, name string) bool {
	if v, ok := args[name].(bool); ok {
		return v
	}
	return false
}

func (s *Server) listResources(ctx context.Context) (any, *rpcError) {
	res := []Resource{}
	if s.resourceLister != nil {
		if listed, err := s.resourceLister(ctx); err == nil && listed != nil {
			// Best-effort: a connection error yields an empty list, not a failure.
			res = listed
		}
	}
	return map[string]any{"resources": res}, nil
}

func (s *Server) readResource(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
		return nil, &rpcError{Code: -32602, Message: "uri is required"}
	}
	if s.resourceReader == nil {
		return nil, &rpcError{Code: -32601, Message: "resources are not supported"}
	}
	c, err := s.resourceReader(ctx, p.URI)
	if err != nil {
		return nil, &rpcError{Code: -32002, Message: err.Error()}
	}
	return map[string]any{
		"contents": []map[string]any{{
			"uri":      c.URI,
			"mimeType": c.MimeType,
			"text":     c.Text,
		}},
	}, nil
}

func (s *Server) promptList() []map[string]any {
	list := make([]map[string]any, 0, len(s.promptOrder))
	for _, name := range s.promptOrder {
		p := s.prompts[name]
		args := p.Arguments
		if args == nil {
			args = []PromptArg{}
		}
		list = append(list, map[string]any{
			"name":        p.Name,
			"description": p.Description,
			"arguments":   args,
		})
	}
	return list
}

func (s *Server) getPrompt(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	prompt, ok := s.prompts[p.Name]
	if !ok {
		return nil, &rpcError{Code: -32602, Message: "unknown prompt: " + p.Name}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]string{}
	}
	result, err := prompt.Handler(ctx, p.Arguments)
	if err != nil {
		return nil, &rpcError{Code: -32603, Message: err.Error()}
	}
	messages := make([]map[string]any, 0, len(result.Messages))
	for _, m := range result.Messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]any{
			"role":    role,
			"content": map[string]any{"type": "text", "text": m.Text},
		})
	}
	return map[string]any{
		"description": result.Description,
		"messages":    messages,
	}, nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// StringArg returns a string argument by name (empty if missing/not a string).
func StringArg(args map[string]any, name string) string {
	if v, ok := args[name].(string); ok {
		return v
	}
	return ""
}
