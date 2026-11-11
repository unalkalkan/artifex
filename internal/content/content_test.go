package content

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

func TestSignedContent(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{BaseURL: "http://artifex.test", DataDir: dir}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes, cfg.Limits.ContentURLTTL = 8<<20, 8<<20, time.Hour
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := service.New(cfg, st, events.NewHub(), zap.NewNop())
	ctx := context.Background()
	acct, _ := svc.AddAccount(ctx, "a@x.test", "A", false)
	a := service.Actor{Account: acct}
	html := `<!doctype html><html><head><title>T</title><link rel="stylesheet" href="../s.css"></head><body>hi</body></html>`
	css := "body{color:red}"
	res, err := svc.Publish(ctx, a, "", "", nil) // no project: error expected
	_ = res
	if _, err := svc.CreateProject(ctx, a, "p", ""); err != nil {
		t.Fatal(err)
	}
	r, err := svc.Publish(ctx, a, "p", "", []service.FileChange{{Path: "f/index.html", Content: &html}, {Path: "s.css", Content: &css}})
	if err != nil {
		t.Fatal(err)
	}
	// A bridge with "</script>" in a string: it must not end the element it is inlined in.
	srv := New(svc, []byte("0123456789abcdef0123456789abcdef"), []byte(`var bridge = "</script>";`))
	mux := http.NewServeMux()
	mux.Handle("/c/", srv)

	get := func(u string) *http.Response {
		u = strings.TrimPrefix(u, "http://artifex.test")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		return rec.Result()
	}
	// The folder serves its index.html, with the bridge injected and the CSP sandbox.
	resp := get(srv.Prefix("p", r.Version) + "f/")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `<head><script>var bridge = "<\/script>";</script><title>`) {
		t.Fatalf("index: %d %s", resp.StatusCode, body)
	}
	// The page's ETag names the bridge too, so a new release isn't hidden behind a cached page.
	if etag := resp.Header.Get("ETag"); !strings.Contains(etag, "-") {
		t.Fatalf("etag: %s", etag)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox allow-scripts") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("csp: %s", csp)
	}
	// A relative asset under the same prefix is signed too.
	resp = get(srv.URL("p", r.Version, "s.css"))
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != css || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	// A large file is streamed, whole and with its length.
	big := strings.Repeat("0123456789abcdef", 300000) // 4.8 MB
	r2, err := svc.Publish(ctx, a, "p", "", []service.FileChange{{Path: "big.bin", Content: &big}})
	if err != nil {
		t.Fatal(err)
	}
	resp = get(srv.URL("p", r2.Version, "big.bin"))
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(body) != len(big) || resp.Header.Get("Content-Length") != "4800000" {
		t.Fatalf("big file: %d %d %q", resp.StatusCode, len(body), resp.Header.Get("Content-Length"))
	}
	// Tampering with the project or the signature fails.
	u := srv.URL("p", r.Version, "s.css")
	if resp := get(strings.Replace(u, "/p/", "/q/", 1)); resp.StatusCode != 403 {
		t.Fatalf("other project: %d", resp.StatusCode)
	}
	i := strings.Index(u, "/c/") + 3
	j := i + strings.Index(u[i:], ".") + 1
	if resp := get(u[:j] + "AAAA" + u[j+4:]); resp.StatusCode != 403 {
		t.Fatalf("bad signature: %d", resp.StatusCode)
	}
	// Expired addresses fail.
	srv.ttl = -time.Minute
	if resp := get(srv.URL("p", r.Version, "s.css")); resp.StatusCode != 403 {
		t.Fatalf("expired: %d", resp.StatusCode)
	}
}
