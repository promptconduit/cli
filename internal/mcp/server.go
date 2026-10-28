// Package mcp is a small Model Context Protocol server over stdio:
// newline-delimited JSON-RPC 2.0 carrying initialize, ping, tools/* and
// prompts/*. It is hand-rolled because the module targets Go 1.21, which the
// official Go SDK doesn't support, and the surface we need is small.
//
// stdout carries protocol messages only; anything diagnostic goes to the
// Logf callback (stderr in practice).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// LatestProtocolVersion is offered when the client asks for a version we don't know.
const LatestProtocolVersion = "2025-06-18"

var supportedVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// maxMessageBytes bounds one inbound line; tool arguments are tiny.
const maxMessageBytes = 4 << 20

// Annotations are hints clients use to decide whether to ask before running
// a tool. All four are always sent: the spec's defaults for an absent
// destructiveHint assume the worst.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// ToolHandler runs a tool. A string result is sent as text; anything else is
// sent as indented JSON text and, when it encodes to an object, also as
// structuredContent. A returned error becomes an isError result the model
// can read and recover from, not a protocol error.
type ToolHandler func(ctx context.Context, args json.RawMessage) (interface{}, error)

// Tool is one callable tool.
type Tool struct {
	Name        string                 `json:"name"`
	Title       string                 `json:"title,omitempty"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	Annotations *Annotations           `json:"annotations,omitempty"`
	Handler     ToolHandler            `json:"-"`
}

// PromptArgument is one named argument of a prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Prompt is a reusable prompt template; clients surface these as slash commands.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
	// Render returns the user-message text for the given arguments.
	Render func(args map[string]string) string `json:"-"`
}

// Server dispatches MCP requests to registered tools and prompts.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Logf         func(format string, args ...interface{})

	tools   map[string]*Tool
	prompts map[string]*Prompt

	writeMu sync.Mutex
	out     io.Writer
}

// NewServer returns an empty server.
func NewServer(name, version, instructions string) *Server {
	return &Server{
		Name:         name,
		Version:      version,
		Instructions: instructions,
		Logf:         func(string, ...interface{}) {},
		tools:        map[string]*Tool{},
		prompts:      map[string]*Prompt{},
	}
}

// AddTool registers a tool; a later tool with the same name replaces it.
func (s *Server) AddTool(t *Tool) { s.tools[t.Name] = t }

// AddPrompt registers a prompt.
func (s *Server) AddPrompt(p *Prompt) { s.prompts[p.Name] = p }

// Tools returns the registered tools sorted by name.
func (s *Server) Tools() []*Tool {
	out := make([]*Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from r and writes responses to w until r reaches EOF
// or ctx is done. Tool calls run concurrently so a slow network call doesn't
// block pings; responses are written whole, one per line.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.out = w
	reader := bufio.NewReaderSize(r, 64*1024)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, tooLarge, err := readLine(reader)
		switch {
		case tooLarge:
			s.writeError(json.RawMessage("null"), codeInvalidRequest, "message too large")
		case len(line) > 0:
			s.handleLine(ctx, line, &wg)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readLine reads one newline-terminated message. An oversized line is
// drained (so the stream stays in sync) and reported via tooLarge.
func readLine(r *bufio.Reader) (line []byte, tooLarge bool, err error) {
	for {
		chunk, isPrefix, err := r.ReadLine()
		if !tooLarge {
			line = append(line, chunk...)
			if len(line) > maxMessageBytes {
				tooLarge, line = true, nil
			}
		}
		if err != nil || !isPrefix {
			return line, tooLarge, err
		}
	}
}

func (s *Server) handleLine(ctx context.Context, line []byte, wg *sync.WaitGroup) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return
	}
	if strings.HasPrefix(trimmed, "[") {
		s.writeError(json.RawMessage("null"), codeInvalidRequest, "batch requests are not supported")
		return
	}
	var req request
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		s.writeError(json.RawMessage("null"), codeParseError, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) > 0 {
			s.writeError(req.ID, codeInvalidRequest, "invalid request")
		}
		return
	}
	// Notifications (no id) get no reply: notifications/initialized,
	// notifications/cancelled and anything else we don't act on.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}

	if req.Method == "tools/call" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.reply(req.ID, s.callTool(ctx, req.Params))
		}()
		return
	}
	s.reply(req.ID, s.dispatch(req))
}

type result struct {
	value interface{}
	err   *rpcError
}

func (s *Server) reply(id json.RawMessage, res result) {
	if res.err != nil {
		s.writeError(id, res.err.Code, res.err.Message)
		return
	}
	s.write(response{JSONRPC: "2.0", ID: id, Result: res.value})
}

func (s *Server) dispatch(req request) result {
	switch req.Method {
	case "initialize":
		return result{value: s.initialize(req.Params)}
	case "ping":
		return result{value: map[string]interface{}{}}
	case "tools/list":
		return result{value: map[string]interface{}{"tools": s.Tools()}}
	case "prompts/list":
		return result{value: map[string]interface{}{"prompts": s.sortedPrompts()}}
	case "prompts/get":
		return s.getPrompt(req.Params)
	default:
		return result{err: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}}
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]interface{} {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(params, &p)
	version := LatestProtocolVersion
	if supportedVersions[p.ProtocolVersion] {
		version = p.ProtocolVersion
	}
	s.Logf("mcp: initialize from %s %s (protocol %s)", p.ClientInfo.Name, p.ClientInfo.Version, version)

	out := map[string]interface{}{
		"protocolVersion": version,
		"capabilities": map[string]interface{}{
			"tools":   map[string]interface{}{"listChanged": false},
			"prompts": map[string]interface{}{"listChanged": false},
		},
		"serverInfo": map[string]interface{}{"name": s.Name, "version": s.Version},
	}
	if s.Instructions != "" {
		out["instructions"] = s.Instructions
	}
	return out
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (res result) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return result{err: &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}}
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		return result{err: &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}}
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}

	defer func() {
		if r := recover(); r != nil {
			s.Logf("mcp: tool %s panicked: %v", p.Name, r)
			res = result{value: errorContent(fmt.Errorf("internal error in %s", p.Name))}
		}
	}()
	out, err := tool.Handler(ctx, args)
	if err != nil {
		return result{value: errorContent(err)}
	}
	content, err := toolContent(out)
	if err != nil {
		return result{err: &rpcError{Code: codeInternalError, Message: err.Error()}}
	}
	return result{value: content}
}

func errorContent(err error) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}

func toolContent(v interface{}) (map[string]interface{}, error) {
	if s, ok := v.(string); ok {
		return map[string]interface{}{
			"content": []interface{}{map[string]interface{}{"type": "text", "text": s}},
		}, nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	out := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": string(data)}},
	}
	if len(data) > 0 && data[0] == '{' {
		out["structuredContent"] = json.RawMessage(data)
	}
	return out, nil
}

func (s *Server) sortedPrompts() []*Prompt {
	out := make([]*Prompt, 0, len(s.prompts))
	for _, p := range s.prompts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) getPrompt(params json.RawMessage) result {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return result{err: &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}}
	}
	prompt, ok := s.prompts[p.Name]
	if !ok {
		return result{err: &rpcError{Code: codeInvalidParams, Message: "unknown prompt: " + p.Name}}
	}
	for _, a := range prompt.Arguments {
		if a.Required && p.Arguments[a.Name] == "" {
			return result{err: &rpcError{Code: codeInvalidParams, Message: "missing required argument: " + a.Name}}
		}
	}
	if p.Arguments == nil {
		p.Arguments = map[string]string{}
	}
	return result{value: map[string]interface{}{
		"description": prompt.Description,
		"messages": []interface{}{map[string]interface{}{
			"role":    "user",
			"content": map[string]interface{}{"type": "text", "text": prompt.Render(p.Arguments)},
		}},
	}}
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) write(resp response) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.Logf("mcp: encode response: %v", err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.out.Write(append(data, '\n')); err != nil {
		s.Logf("mcp: write response: %v", err)
	}
}
