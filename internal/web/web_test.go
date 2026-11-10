package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

func TestPagesAndProtections(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{BaseURL: base, DataDir: dir, DevAuth: true, Admins: []string{"admin@x.test"}}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes = 1<<20, 1<<20
	cfg.Limits.ContentURLTTL, cfg.Limits.SessionTTL, cfg.Limits.RequestsPerMinute = time.Hour, time.Hour, 1000
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := service.New(cfg, st, events.NewHub(), zap.NewNop())
	ws, err := New(svc, []byte("0123456789abcdef0123456789abcdef"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = ws.Handler(Options{})
	srv.Start()
	defer srv.Close()

	ctx := context.Background()
	acct, _ := svc.AddAccount(ctx, "dev@x.test", "Dev", false)
	a := service.Actor{Account: acct, AgentLabel: "Claude"}
	html := `<!doctype html><html><head><title>Wallet</title><meta name="artifact" content="width=1440"></head><body><h1 id="w">Wallet</h1></body></html>`
	r1, err := svc.UploadArtifact(ctx, a, "demo", "screens/wallet.html", &html, nil, "", "Demo", "")
	if err != nil {
		t.Fatal(err)
	}
	css := "h1{}"
	svc.UploadArtifact(ctx, a, "demo", "s.css", &css, nil, "", "", "")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(p string) (*http.Response, string) {
		resp, err := client.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// Signed out: the sign-in page renders; other pages redirect to it, the API says 401.
	if resp, body := get("/login"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Development sign-in is on") || !strings.Contains(body, `class="theme-dark theme-slate"`) {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	if resp, _ := get("/p/demo"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("signed out: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := get("/api/projects/demo/threads"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api signed out: %d", resp.StatusCode)
	}
	// Sign in.
	if resp, _ := get("/auth/dev?email=dev@x.test"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dev sign-in: %d", resp.StatusCode)
	}
	// Every page renders.
	pages := []string{"/", "/tokens", "/p/demo", "/p/demo/screens", "/p/demo/screens/wallet.html", "/p/demo/s.css",
		"/p/demo/-/history", "/p/demo/-/history?path=screens/wallet.html", "/p/demo/-/members",
		"/p/demo/-/compare?to=" + r1.Version + "&path=screens/wallet.html", "/p/demo/screens/wallet.html?v=" + r1.Version}
	var csrf string
	for _, p := range pages {
		resp, body := get(p)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body[:min(len(body), 300)])
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src "+base+"/static/;") || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s: missing security headers", p)
		}
		if m := regexp.MustCompile(`data-csrf="([^"]+)"`).FindStringSubmatch(body); m != nil {
			csrf = m[1]
		}
	}
	if resp, _ := get("/admin/accounts"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member on admin page: %d", resp.StatusCode)
	}
	if resp, _ := get("/p/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing project: %d", resp.StatusCode)
	}
	// The artifact page points at a signed content address that serves the
	// file with the sandbox CSP.
	_, body := get("/p/demo/screens/wallet.html")
	m := regexp.MustCompile(`id="frame"[^>]*src="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no frame")
	}
	resp, cbody := get(strings.TrimPrefix(strings.ReplaceAll(m[1], "&amp;", "&"), base))
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(cbody, "/static/bridge.js") {
		t.Fatalf("content: %d %s", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}

	post := func(p, body string, hdr map[string]string) int {
		req, _ := http.NewRequest("POST", base+p, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	thread := `{"path":"screens/wallet.html","body":"Bigger title","anchor":{"selector":"#w","text":"Wallet"}}`
	if code := post("/api/projects/demo/threads", thread, nil); code != http.StatusForbidden {
		t.Fatalf("no CSRF token: %d", code)
	}
	if code := post("/api/projects/demo/threads", thread, map[string]string{"X-CSRF-Token": csrf, "Origin": "http://evil.test"}); code != http.StatusForbidden {
		t.Fatalf("other origin: %d", code)
	}
	if code := post("/api/projects/demo/threads", thread, map[string]string{"X-CSRF-Token": csrf, "Origin": base}); code != http.StatusCreated {
		t.Fatalf("create thread: %d", code)
	}
	_, list := get("/api/projects/demo/threads?path=screens/wallet.html")
	if !strings.Contains(list, "Bigger title") {
		t.Fatalf("threads: %s", list)
	}
	// Forms need the CSRF field.
	form := url.Values{"action": {"create"}, "label": {"Claude"}}
	req, _ := http.NewRequest("POST", base+"/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := client.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("form without csrf: %d", resp.StatusCode)
	}
	form.Set("csrf", csrf)
	req, _ = http.NewRequest("POST", base+"/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "afx_") {
		t.Fatalf("token create: %d", resp.StatusCode)
	}

	// An Admin sees the accounts page.
	jar2, _ := cookiejar.New(nil)
	client.Jar = jar2
	get("/auth/dev?email=admin@x.test")
	if resp, body := get("/admin/accounts"); resp.StatusCode != 200 || !strings.Contains(body, "dev@x.test") {
		t.Fatalf("admin page: %d", resp.StatusCode)
	}
	if resp, _ := get("/p/demo"); resp.StatusCode != 200 {
		t.Fatalf("admin sees every project: %d", resp.StatusCode)
	}
}

// Static files: compressed when the browser accepts it, and revalidated by ETag.
func TestStaticFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{BaseURL: "http://artifex.test", DataDir: dir}
	cfg.Limits.ContentURLTTL, cfg.Limits.SessionTTL, cfg.Limits.RequestsPerMinute = time.Hour, time.Hour, 1000
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws, err := New(service.New(cfg, st, events.NewHub(), zap.NewNop()), []byte("0123456789abcdef0123456789abcdef"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	h := ws.Handler(Options{})
	const name = "vendor/tabler/tabler.min.css"
	want, _ := staticFS.ReadFile("static/" + name)
	do := func(hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/static/"+name+"?v="+ws.assets[name], nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do(map[string]string{"Accept-Encoding": "br, gzip"})
	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip: %d %q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, want) || rec.Body.Len() > len(want)/4 {
		t.Fatalf("gzip body: %d bytes from %d, unpacks to %d", rec.Body.Len(), len(want), len(got))
	}
	if rec := do(map[string]string{"Accept-Encoding": "gzip;q=0"}); rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatal("gzip sent to a browser that refused it")
	}
	etag := rec.Header().Get("ETag")
	if rec := do(map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified || etag == "" {
		t.Fatalf("revalidation: %d %q", rec.Code, etag)
	}
	// Icons: a known one draws inline, an unknown one fails the page.
	if svg, err := ws.icon("folder", "text-primary"); err != nil || !strings.HasPrefix(string(svg), "<svg") || !strings.Contains(string(svg), `class="icon text-primary"`) {
		t.Fatalf("icon: %v %s", err, svg)
	}
	if _, err := ws.icon("no-such-icon"); err == nil {
		t.Fatal("unknown icon accepted")
	}
}
