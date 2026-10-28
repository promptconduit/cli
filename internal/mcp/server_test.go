package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testServer() *Server {
	s := NewServer("pc", "1.2.3", "use me")
	s.AddTool(&Tool{
		Name: "echo", Description: "Echo args.",
		InputSchema: map[string]interface{}{"type": "object"},
		Annotations: &Annotations{ReadOnlyHint: true},
		Handler: func(_ context.Context, args json.RawMessage) (interface{}, error) {
			var m map[string]interface{}
			_ = json.Unmarshal(args, &m)
			return m, nil
		},
	})
	s.AddTool(&Tool{
		Name: "text", Description: "Plain text.", InputSchema: map[string]interface{}{"type": "object"},
		Handler: func(context.Context, json.RawMessage) (interface{}, error) { return "hello", nil },
	})
	s.AddTool(&Tool{
		Name: "fail", Description: "Fails.", InputSchema: map[string]interface{}{"type": "object"},
		Handler: func(context.Context, json.RawMessage) (interface{}, error) { return nil, errors.New("not signed in") },
	})
	s.AddTool(&Tool{
		Name: "boom", Description: "Panics.", InputSchema: map[string]interface{}{"type": "object"},
		Handler: func(context.Context, json.RawMessage) (interface{}, error) { panic("oops") },
	})
	s.AddPrompt(&Prompt{
		Name: "review", Arguments: []PromptArgument{{Name: "who", Required: true}},
		Render: func(a map[string]string) string { return "review " + a["who"] },
	})
	return s
}

// run feeds lines to a server and returns its replies keyed by id.
func run(t *testing.T, s *Server, lines ...string) (map[string]map[string]interface{}, []map[string]interface{}) {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]interface{}{}
	var all []map[string]interface{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("non-JSON output line %q: %v", sc.Text(), err)
		}
		all = append(all, m)
		id, _ := json.Marshal(m["id"])
		byID[string(id)] = m
	}
	return byID, all
}

func resultOf(t *testing.T, m map[string]interface{}) map[string]interface{} {
	t.Helper()
	r, ok := m["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	return r
}

func errCode(m map[string]interface{}) float64 {
	e, _ := m["error"].(map[string]interface{})
	c, _ := e["code"].(float64)
	return c
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	got, _ := run(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"t"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`,
	)
	r1 := resultOf(t, got["1"])
	if r1["protocolVersion"] != "2025-03-26" || r1["instructions"] != "use me" {
		t.Errorf("init 1 = %v", r1)
	}
	info := r1["serverInfo"].(map[string]interface{})
	if info["name"] != "pc" || info["version"] != "1.2.3" {
		t.Errorf("serverInfo = %v", info)
	}
	caps := r1["capabilities"].(map[string]interface{})
	if caps["tools"] == nil || caps["prompts"] == nil {
		t.Errorf("capabilities = %v", caps)
	}
	if v := resultOf(t, got["2"])["protocolVersion"]; v != LatestProtocolVersion {
		t.Errorf("unknown version → %v", v)
	}
}

func TestNotificationsGetNoReply(t *testing.T) {
	_, all := run(t, testServer(),
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	if len(all) != 1 || all[0]["id"] != float64(7) {
		t.Fatalf("want only the ping reply, got %v", all)
	}
}

func TestToolsListSortedWithAnnotations(t *testing.T) {
	got, _ := run(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := resultOf(t, got["1"])["tools"].([]interface{})
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]interface{})["name"].(string))
	}
	if strings.Join(names, ",") != "boom,echo,fail,text" {
		t.Errorf("names = %v", names)
	}
	echo := tools[1].(map[string]interface{})
	ann := echo["annotations"].(map[string]interface{})
	if ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
		t.Errorf("annotations = %v", ann)
	}
	if _, ok := echo["Handler"]; ok {
		t.Error("handler leaked into JSON")
	}
}

func TestToolsCall(t *testing.T) {
	got, _ := run(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"text"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fail","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"boom"}}`,
	)

	r1 := resultOf(t, got["1"])
	if sc := r1["structuredContent"].(map[string]interface{}); sc["a"] != float64(1) {
		t.Errorf("structured = %v", sc)
	}
	text := r1["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, `"a": 1`) {
		t.Errorf("text = %q", text)
	}

	r2 := resultOf(t, got["2"])
	if r2["structuredContent"] != nil || r2["content"].([]interface{})[0].(map[string]interface{})["text"] != "hello" {
		t.Errorf("text tool = %v", r2)
	}

	r3 := resultOf(t, got["3"])
	if r3["isError"] != true || r3["content"].([]interface{})[0].(map[string]interface{})["text"] != "not signed in" {
		t.Errorf("error tool = %v", r3)
	}

	if errCode(got["4"]) != codeInvalidParams {
		t.Errorf("unknown tool = %v", got["4"])
	}
	if r5 := resultOf(t, got["5"]); r5["isError"] != true {
		t.Errorf("panic = %v", r5)
	}
}

func TestPrompts(t *testing.T) {
	got, _ := run(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"review","arguments":{"who":"me"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"review"}}`,
	)
	if ps := resultOf(t, got["1"])["prompts"].([]interface{}); len(ps) != 1 {
		t.Errorf("prompts = %v", ps)
	}
	msg := resultOf(t, got["2"])["messages"].([]interface{})[0].(map[string]interface{})
	if msg["role"] != "user" || msg["content"].(map[string]interface{})["text"] != "review me" {
		t.Errorf("message = %v", msg)
	}
	if errCode(got["3"]) != codeInvalidParams {
		t.Errorf("missing arg = %v", got["3"])
	}
}

func TestProtocolErrors(t *testing.T) {
	got, all := run(t, testServer(),
		`not json`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":9,"method":"resources/read"}`,
		`{"jsonrpc":"1.0","id":10,"method":"ping"}`,
	)
	if len(all) != 4 {
		t.Fatalf("replies = %v", all)
	}
	if errCode(all[0]) != codeParseError || errCode(all[1]) != codeInvalidRequest {
		t.Errorf("parse/batch = %v / %v", all[0], all[1])
	}
	if errCode(got["9"]) != codeMethodNotFound || errCode(got["10"]) != codeInvalidRequest {
		t.Errorf("method/version = %v / %v", got["9"], got["10"])
	}
}

func TestOversizedMessageKeepsStreamInSync(t *testing.T) {
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", maxMessageBytes+10) + `"}}`
	got, all := run(t, testServer(), big, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(all) != 2 || errCode(all[0]) != codeInvalidRequest {
		t.Fatalf("replies = %d, first = %v", len(all), all[0])
	}
	if _, ok := got["2"]["result"]; !ok {
		t.Errorf("ping after oversized message = %v", got["2"])
	}
}
