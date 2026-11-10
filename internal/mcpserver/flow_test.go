package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/content"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/render"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
	"github.com/unalkalkan/artifex/internal/upload"
)

// An agent sends files from its disk to an upload address, publishes them by
// their ids, and looks at the result.
func TestMCPUploadPublishScreenshot(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{BaseURL: base, DataDir: dir}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes = 1<<20, 2<<20
	cfg.Limits.ContentURLTTL, cfg.Limits.UploadURLTTL = time.Hour, time.Minute
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := service.New(cfg, st, events.NewHub(), zap.NewNop())
	key := []byte("0123456789abcdef0123456789abcdef")
	cs := content.New(svc, key, base+"/static/bridge.js")
	mux := http.NewServeMux()
	mux.Handle("/c/", cs)
	mux.HandleFunc("/static/bridge.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
	})
	mux.Handle("/u/", upload.New(svc, key, zap.NewNop()))
	var shots render.Renderer
	if p := render.FindChromium(); p != "" {
		shots = &render.Browser{Path: p, Upstream: mux}
	}
	mux.Handle("/mcp", Handler(svc, zap.NewNop(), Options{Uploads: upload.New(svc, key, zap.NewNop()), Renderer: shots, ContentURL: cs.URL}))
	srv.Config.Handler = mux
	srv.Start()
	defer srv.Close()

	ctx := context.Background()
	acct, _ := svc.AddAccount(ctx, "dev@x.test", "Dev", false)
	tok, _ := svc.CreateToken(ctx, acct, "Claude")
	s := connect(t, base+"/mcp", tok.Secret)

	out, isErr, text := call(t, s, "create_upload", map[string]any{"project": "shots", "title": "Shots"})
	if isErr || out["project_created"] != true {
		t.Fatalf("create_upload: %s", text)
	}
	addr := out["url"].(string)
	var changes []any
	for path, body := range map[string]string{
		"screens/home.html": `<!doctype html><html><head><title>Home</title><meta name="artifact" content="width=800, height=600">
<link rel="stylesheet" href="../assets/app.css"><script src="https://cdn.example.com/lib.js"></script></head>
<body><h1>Home</h1></body></html>`,
		"assets/app.css": `body{margin:0;background:#0a7} h1{color:#fff}`,
	} {
		req, _ := http.NewRequest(http.MethodPut, addr+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var ans struct {
			Changes []any `json:"changes"`
		}
		json.NewDecoder(resp.Body).Decode(&ans)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d", path, resp.StatusCode)
		}
		changes = append(changes, ans.Changes...) // passed to publish as they are
	}
	out, isErr, text = call(t, s, "publish", map[string]any{"project": "shots", "message": "From disk", "changes": changes})
	if isErr {
		t.Fatalf("publish: %s", text)
	}
	if ws := out["warnings"]; ws == nil || !strings.Contains(ws.([]any)[0].(string), "https://cdn.example.com/lib.js is on another host") {
		t.Fatalf("warnings: %v", out["warnings"])
	}
	// publish creates a project too.
	if out, isErr, text := call(t, s, "publish", map[string]any{"project": "made-by-publish", "changes": []any{map[string]any{"path": "a.html", "content": "a"}}}); isErr || out["project_created"] != true {
		t.Fatalf("publish creating: %s", text)
	}
	// Publishing an upload over an existing file without its base: the
	// conflict names the current blob, and sends the agent to read_file
	// rather than to content that isn't there.
	_, isErr, text = call(t, s, "publish", map[string]any{"project": "shots", "changes": changes[:1]})
	if !isErr || !strings.Contains(text, "current_blob") || !strings.Contains(text, "read_file shows it") || strings.Contains(text, "current_content") {
		t.Fatalf("upload conflict: %s", text)
	}

	if shots == nil {
		t.Skip("no Chromium on PATH: screenshot not checked")
	}
	tl, _ := s.ListTools(ctx, nil)
	found := false
	for _, tool := range tl.Tools {
		found = found || tool.Name == "screenshot"
	}
	if !found {
		t.Fatal("no screenshot tool")
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "screenshot", Arguments: map[string]any{"project": "shots", "path": "screens/home.html"}})
	if err != nil || res.IsError {
		t.Fatalf("screenshot: %v %+v", err, res)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/png" {
		t.Fatalf("content: %+v", res.Content)
	}
	pic, err := png.Decode(bytes.NewReader(img.Data))
	if err != nil || pic.Bounds().Dx() != 800 || pic.Bounds().Dy() != 600 {
		t.Fatalf("picture: %v %v", err, pic.Bounds())
	}
	// The stylesheet came from the project: the background is its green.
	if r, g, b, _ := pic.At(400, 500).RGBA(); r>>8 != 0 || g>>8 != 0xaa || b>>8 != 0x77 {
		t.Fatalf("pixel %d,%d,%d", r>>8, g>>8, b>>8)
	}
	report := res.Content[1].(*mcp.TextContent).Text
	for _, want := range []string{"800 px wide (its own width)", "600 px tall (its own height)", "https://cdn.example.com/lib.js"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
	// A commenter may look; a stranger may not.
	other, _ := svc.AddAccount(ctx, "o@x.test", "O", false)
	ot, _ := svc.CreateToken(ctx, other, "Claude")
	s2 := connect(t, base+"/mcp", ot.Secret)
	if res, _ := s2.CallTool(ctx, &mcp.CallToolParams{Name: "screenshot", Arguments: map[string]any{"project": "shots", "path": "screens/home.html"}}); !res.IsError {
		t.Fatal("a stranger got a screenshot")
	}
}
