package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool, string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return out, res.IsError, text
}

func TestMCPEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{BaseURL: "http://artifex.test", DataDir: dir}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes = 1<<20, 1<<20
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := service.New(cfg, st, events.NewHub(), zap.NewNop())
	ctx := context.Background()
	acct, _ := svc.AddAccount(ctx, "dev@x.test", "Dev", false)
	tok, _ := svc.CreateToken(ctx, acct, "Claude")
	srv := httptest.NewServer(Handler(svc, zap.NewNop(), Options{}))
	defer srv.Close()

	// No token: refused.
	resp, _ := http.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}

	s := connect(t, srv.URL, tok.Secret)
	tl, err := s.ListTools(ctx, nil)
	if err != nil || len(tl.Tools) < 19 {
		t.Fatalf("tools: %v %d", err, len(tl.Tools))
	}
	out, isErr, text := call(t, s, "upload_artifact", map[string]any{"project": "mockups", "path": "index.html", "content": "<title>Home</title><h1>Hello</h1>", "title": "Mockups"})
	if isErr || out["project_created"] != true {
		t.Fatalf("upload: %v %s", isErr, text)
	}
	files := out["files"].([]any)
	blob := files[0].(map[string]any)["blob"].(string)
	version := out["version"].(string)
	if !strings.HasPrefix(files[0].(map[string]any)["url"].(string), "http://artifex.test/p/mockups/index.html") {
		t.Fatalf("url: %v", files[0])
	}

	// Conflict: no base for an existing file.
	_, isErr, text = call(t, s, "upload_artifact", map[string]any{"project": "mockups", "path": "index.html", "content": "x"})
	if !isErr || !strings.Contains(text, "current_content") || !strings.Contains(text, "Hello") {
		t.Fatalf("conflict: %v %s", isErr, text)
	}
	// publish with the base, plus a stylesheet.
	out, isErr, text = call(t, s, "publish", map[string]any{"project": "mockups", "message": "Style", "changes": []any{
		map[string]any{"path": "index.html", "base": blob, "content": `<link rel="stylesheet" href="s.css"><title>Home</title><h1>Hello</h1>`},
		map[string]any{"path": "s.css", "content": "h1{color:teal}"},
	}})
	if isErr {
		t.Fatalf("publish: %s", text)
	}
	out, isErr, _ = call(t, s, "read_file", map[string]any{"project": "mockups", "path": "s.css"})
	if isErr || out["content"] != "h1{color:teal}" {
		t.Fatalf("read: %v", out)
	}
	out, _, _ = call(t, s, "history", map[string]any{"project": "mockups"})
	if vs := out["versions"].([]any); len(vs) != 3 { // project.yaml, upload, publish
		t.Fatalf("history: %v", vs)
	}
	// Threads: the agent starts one, replies, resolves with a version.
	out, isErr, text = call(t, s, "create_thread", map[string]any{"project": "mockups", "path": "index.html", "body": "Is teal right?", "anchor": map[string]any{"selector": "h1", "text": "Hello"}})
	if isErr {
		t.Fatalf("create_thread: %s", text)
	}
	id := out["id"].(float64)
	out, _, _ = call(t, s, "list_threads", map[string]any{"state": "open"})
	if ts := out["threads"].([]any); len(ts) != 1 {
		t.Fatalf("threads: %v", ts)
	}
	out, isErr, text = call(t, s, "get_thread", map[string]any{"id": id})
	if isErr || !strings.Contains(out["excerpt"].(string), "Hello") {
		t.Fatalf("get_thread: %s", text)
	}
	if _, isErr, text = call(t, s, "resolve_thread", map[string]any{"id": id, "version": version, "body": "Done"}); isErr {
		t.Fatalf("resolve: %s", text)
	}
	// Errors are readable, not internal.
	_, isErr, text = call(t, s, "read_file", map[string]any{"project": "nope", "path": "x.html"})
	if !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("missing project: %s", text)
	}
	_, isErr, text = call(t, s, "upload_artifact", map[string]any{"project": "Bad Name", "path": "x.html", "content": "x"})
	if !isErr || !strings.Contains(text, "lowercase") {
		t.Fatalf("bad id: %s", text)
	}
	// A commenter's token can read but not publish.
	other, _ := svc.AddAccount(ctx, "c@x.test", "C", false)
	svc.SetMember(ctx, service.Actor{Account: acct}, "mockups", "c@x.test", "commenter")
	ot, _ := svc.CreateToken(ctx, other, "Claude")
	s2 := connect(t, srv.URL, ot.Secret)
	if _, isErr, _ := call(t, s2, "read_file", map[string]any{"project": "mockups", "path": "index.html"}); isErr {
		t.Fatal("commenter can't read")
	}
	if _, isErr, text := call(t, s2, "upload_artifact", map[string]any{"project": "mockups", "path": "y.html", "content": "y"}); !isErr || !strings.Contains(text, "editor") {
		t.Fatalf("commenter published: %s", text)
	}
}

// A publish up to limits.max_publish_bytes goes through /mcp, even though its
// base64 request is a third larger and above the MCP SDK's 4 MiB default.
func TestMCPLargePublish(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{BaseURL: "http://artifex.test", DataDir: dir}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes = 8<<20, 8<<20
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := service.New(cfg, st, events.NewHub(), zap.NewNop())
	ctx := context.Background()
	acct, _ := svc.AddAccount(ctx, "dev@x.test", "Dev", false)
	tok, _ := svc.CreateToken(ctx, acct, "Claude")
	srv := httptest.NewServer(Handler(svc, zap.NewNop(), Options{}))
	defer srv.Close()
	s := connect(t, srv.URL, tok.Secret)
	if _, isErr, text := call(t, s, "create_project", map[string]any{"project": "big"}); isErr {
		t.Fatalf("create_project: %s", text)
	}

	file := base64.StdEncoding.EncodeToString(make([]byte, 7<<20)) // about 9.3 MiB as base64
	_, isErr, text := call(t, s, "publish", map[string]any{"project": "big", "message": "Big", "changes": []any{
		map[string]any{"path": "assets/big.bin", "content_base64": file},
	}})
	if isErr {
		t.Fatalf("publish: %s", text)
	}
	// Over the publish limit, in a request still under the request limit
	// (text isn't base64): the service refuses it with a readable error.
	text5 := strings.Repeat("a", 5<<20)
	_, isErr, text = call(t, s, "publish", map[string]any{"project": "big", "message": "Bigger", "changes": []any{
		map[string]any{"path": "a.css", "content": text5},
		map[string]any{"path": "b.css", "content": text5},
	}})
	if !isErr || !strings.Contains(text, "the limit is") {
		t.Fatalf("over the limit: %v %s", isErr, text)
	}
}
