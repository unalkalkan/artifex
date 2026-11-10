package render

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

const prefix = "/c/tok/demo/0123456789abcdef0123456789abcdef01234567/"

// site is a stand-in for Artifex's content server: the same sandbox and
// policy, which allows only its own origin.
func site(origin string, files map[string]string) http.Handler {
	csp := "default-src " + origin + " data: blob:; script-src " + origin + " 'unsafe-inline'; style-src " + origin + " 'unsafe-inline'; img-src " + origin + " data:; sandbox allow-scripts"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", csp)
		if strings.HasSuffix(r.URL.Path, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.Write([]byte(body))
	})
}

func browser(t *testing.T, h http.Handler) *Browser {
	t.Helper()
	p := FindChromium()
	if p == "" {
		t.Skip("no Chromium on PATH")
	}
	return &Browser{Path: p, Upstream: h}
}

func picture(t *testing.T, res Result) (int, int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(res.PNG))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	return img.Bounds().Dx(), img.Bounds().Dy()
}

func TestRenderFollowsContent(t *testing.T) {
	origin := "https://artifex.test"
	b := browser(t, site(origin, map[string]string{
		"index.html": `<!doctype html><title>T</title>
<link rel="stylesheet" href="https://cdn.example.com/x.css">
<script src="app.js"></script>
<style>body{margin:0} .tall{height:2300px;background:linear-gradient(#08f,#f80)}</style>
<div class="tall"></div><img src="missing.png">`,
		"app.js": `throw new Error("boom")`,
	}))
	res, err := b.Render(context.Background(), Request{URL: origin + prefix + "index.html", Width: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// The broken image's inline box adds a few pixels below the div.
	if res.Height < 2300 || res.Height > 2330 {
		t.Fatalf("frame height %d, want about 2300", res.Height)
	}
	if w, h := picture(t, res); w != 1000 || h != PieceHeight || res.ShotY != 0 {
		t.Fatalf("picture %dx%d at %d", w, h, res.ShotY)
	}
	if !strings.Contains(strings.Join(res.Blocked, " "), "https://cdn.example.com/x.css") {
		t.Fatalf("blocked: %v", res.Blocked)
	}
	if strings.Join(res.Missing, " ") != "missing.png" {
		t.Fatalf("missing: %v", res.Missing)
	}
	if !strings.Contains(strings.Join(res.Errors, " "), "boom") {
		t.Fatalf("errors: %v", res.Errors)
	}
	// The rest, from y.
	res2, err := b.Render(context.Background(), Request{URL: origin + prefix + "index.html", Width: 1000, Y: PieceHeight})
	if err != nil {
		t.Fatal(err)
	}
	if _, h := picture(t, res2); h != res.Height-PieceHeight || res2.ShotY != PieceHeight {
		t.Fatalf("second piece %d at %d", h, res2.ShotY)
	}
	if _, err := b.Render(context.Background(), Request{URL: origin + prefix + "index.html", Y: 99999}); err == nil {
		t.Fatal("y past the end accepted")
	}
}

func TestRenderWindowSizedPage(t *testing.T) {
	origin := "https://artifex.test"
	// 100vh plus the body's margins: it grows with its frame, without end.
	page := `<!doctype html><title>T</title><style>body{margin:8px} main{height:100vh;background:#123}</style><main></main>`
	b := browser(t, site(origin, map[string]string{"a.html": page}))
	res, err := b.Render(context.Background(), Request{URL: origin + prefix + "a.html", Width: 1440})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Locked || res.Height != LockHeight {
		t.Fatalf("locked %v at %d", res.Locked, res.Height)
	}
	// With its own height it is exactly that tall, and scrolls inside.
	res, err = b.Render(context.Background(), Request{URL: origin + prefix + "a.html", Width: 1440, Height: 900, Y: 10})
	if err != nil {
		t.Fatal(err)
	}
	if w, h := picture(t, res); w != 1440 || h != 900 || res.Locked || res.ContentHeight != 916 || res.ShotY != 10 {
		t.Fatalf("fixed: %dx%d locked %v content %d y %d", w, h, res.Locked, res.ContentHeight, res.ShotY)
	}
}

func TestRemote(t *testing.T) {
	origin := "https://artifex.test"
	up := site(origin, map[string]string{"a.html": `<!doctype html><title>T</title><p>hi`})
	b := browser(t, up)
	srv := httptest.NewServer(Handler(b, zap.NewNop()))
	defer srv.Close()
	r := &Remote{URL: srv.URL}
	res, err := r.Render(context.Background(), Request{URL: origin + prefix + "a.html", Width: 800, Height: 600})
	if err != nil {
		t.Fatal(err)
	}
	if w, h := picture(t, res); w != 800 || h != 600 {
		t.Fatalf("%dx%d", w, h)
	}
	var inv *Invalid
	if _, err := r.Render(context.Background(), Request{URL: "https://elsewhere.test/x"}); err == nil || !asInvalid(err, &inv) {
		t.Fatalf("bad url: %v", err)
	}
}

func asInvalid(err error, target **Invalid) bool {
	i, ok := err.(*Invalid)
	if ok {
		*target = i
	}
	return ok
}
