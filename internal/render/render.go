// Package render draws artifacts in headless Chromium, for the screenshot
// tool (design decision 24), so an agent sees what people see.
//
// A render opens the artifact's signed content address in a fresh browser
// with a fresh profile, and answers every request the page makes itself:
// requests to Artifex's origin are served by Upstream, with the content
// policy and sandbox of every content response (design decisions 10 and 11);
// anything else is refused, as the policy would refuse it in a viewer's
// browser. So the browser needs no network at all.
//
// Where the browser runs is the operator's choice. In production it is its
// own container (`artifex renderer`), with no volumes and no secrets: a page
// that broke out of the browser would find nothing there (decision 24).
package render

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The frame sizes Artifex's canvas uses (internal/web/static/app.js,
// initCanvas): a frame starts at StartHeight and follows its content; a page
// that keeps growing with its frame (100vh) stops at LockHeight.
const (
	DefaultWidth = 1440
	StartHeight  = 900
	LockHeight   = 1200
	// PieceHeight is the most a picture of a frame that follows its content
	// shows at once; y asks for the next piece.
	PieceHeight = 1600
	// MaxFrameHeight is the tallest frame a render draws.
	MaxFrameHeight = 16000
)

// Request is one render.
type Request struct {
	URL    string `json:"url"`    // the artifact's signed content address
	Width  int    `json:"width"`  // the viewport's width
	Height int    `json:"height"` // the viewport's height; 0: the frame follows the content
	Y      int    `json:"y"`      // where the picture starts, in the artifact's pixels
}

// Result is what a render saw.
type Result struct {
	PNG []byte `json:"png"`
	// Width and Height are the viewport the artifact was drawn in.
	Width  int `json:"width"`
	Height int `json:"height"`
	// ContentHeight is the document's own height in that viewport.
	ContentHeight int `json:"content_height"`
	// Locked: the page grew with its frame, so the frame stopped at LockHeight.
	Locked bool `json:"locked,omitempty"`
	// ShotY and ShotHeight are the part of the artifact the picture shows.
	ShotY      int `json:"shot_y"`
	ShotHeight int `json:"shot_height"`
	// Blocked are loads the content policy refused: files on other hosts.
	Blocked []string `json:"blocked,omitempty"`
	// Missing are files of the project the page asked for that don't exist.
	Missing []string `json:"missing,omitempty"`
	// Errors are script errors and the page's console errors.
	Errors []string `json:"errors,omitempty"`
}

// Renderer draws artifacts.
type Renderer interface {
	Render(ctx context.Context, req Request) (Result, error)
}

// Invalid is a request a renderer refuses, with a message for the agent.
type Invalid struct{ Msg string }

func (e *Invalid) Error() string { return e.Msg }

func (req *Request) check() error {
	if req.Width == 0 {
		req.Width = DefaultWidth
	}
	switch {
	case req.Width < 200 || req.Width > 8000:
		return &Invalid{"width must be 200 to 8000 pixels"}
	case req.Height != 0 && (req.Height < 100 || req.Height > MaxFrameHeight):
		return &Invalid{fmt.Sprintf("height must be 100 to %d pixels", MaxFrameHeight)}
	case req.Y < 0:
		return &Invalid{"y can't be negative"}
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !strings.HasPrefix(u.Path, "/c/") {
		return &Invalid{"url must be an artifact's content address"}
	}
	return nil
}

// FindChromium looks for a Chromium or Chrome on PATH.
func FindChromium() string {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome-stable", "google-chrome", "chrome", "chrome-headless-shell", "headless_shell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// Browser renders with a Chromium on this host, one browser process per
// render, so no state passes from one render to the next.
type Browser struct {
	// Path is the Chromium binary.
	Path string
	// NoSandbox turns off Chromium's own sandbox, which needs Linux user
	// namespaces that containers usually lack. Only where the container is
	// the sandbox: the renderer's own (decision 24).
	NoSandbox bool
	// Upstream serves Artifex's origin: content under /c/ and the bridge
	// under /static/.
	Upstream http.Handler

	once sync.Once
	slot chan struct{}
}

// Parallel is how many renders a Browser runs at once; others wait.
const Parallel = 2

// Timeout bounds one render.
const Timeout = 45 * time.Second

func (b *Browser) Render(ctx context.Context, req Request) (Result, error) {
	if err := req.check(); err != nil {
		return Result{}, err
	}
	b.once.Do(func() { b.slot = make(chan struct{}, Parallel) })
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	select {
	case b.slot <- struct{}{}:
		defer func() { <-b.slot }()
	case <-ctx.Done():
		return Result{}, errors.New("the renderer is busy; try again")
	}
	r := &run{b: b, req: req, loaded: make(chan struct{}, 1)}
	return r.do(ctx)
}

// run is one render.
type run struct {
	b       *Browser
	req     Request
	origin  string
	prefix  string // the artifact's version folder: /c/<token>/<project>/<commit>/
	c       *conn
	session string
	loaded  chan struct{}
	pending atomic.Int64
	last    atomic.Int64 // unix ms of the last request that finished

	mu      sync.Mutex
	blocked map[string]bool
	missing map[string]bool
	errs    []string
}

func (r *run) do(ctx context.Context) (Result, error) {
	u, _ := url.Parse(r.req.URL)
	r.origin = u.Scheme + "://" + u.Host
	if parts := strings.SplitN(strings.TrimPrefix(u.Path, "/c/"), "/", 4); len(parts) >= 3 {
		r.prefix = "/c/" + strings.Join(parts[:3], "/") + "/"
	}
	r.blocked, r.missing = map[string]bool{}, map[string]bool{}

	dir, err := os.MkdirTemp("", "artifex-render-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	toBrowser, ourEnd, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	fromBrowser, theirEnd, err := os.Pipe()
	if err != nil {
		toBrowser.Close()
		ourEnd.Close()
		return Result{}, err
	}
	args := []string{
		"--headless", "--remote-debugging-pipe", "--user-data-dir=" + filepath.Join(dir, "profile"),
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "--disable-dev-shm-usage",
		"--disable-extensions", "--disable-sync", "--disable-background-networking", "--disable-component-update",
		"--disable-default-apps", "--disable-features=Translate,MediaRouter,OptimizationHints,InterestFeedContentSuggestions",
		"--metrics-recording-only", "--no-pings", "--mute-audio", "--password-store=basic", "--use-mock-keychain",
		"--force-color-profile=srgb", "--deny-permission-prompts", "--hide-crash-restore-bubble",
	}
	if r.b.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(r.b.Path, args...)
	cmd.ExtraFiles = []*os.File{toBrowser, theirEnd} // fd 3: it reads; fd 4: it writes
	cmd.Env = append(os.Environ(), "HOME="+dir)
	var stderr tail
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		toBrowser.Close()
		ourEnd.Close()
		fromBrowser.Close()
		theirEnd.Close()
		return Result{}, fmt.Errorf("starting %s: %w", r.b.Path, err)
	}
	toBrowser.Close()
	theirEnd.Close()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	defer func() {
		ourEnd.Close()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
		fromBrowser.Close()
	}()
	r.c = newConn(ourEnd, fromBrowser, r.event)

	res, err := r.draw(ctx)
	if err != nil {
		select {
		case <-r.c.done:
			if s := strings.TrimSpace(stderr.String()); s != "" {
				err = fmt.Errorf("%w; the browser said: %s", err, s)
			}
		default:
		}
		return Result{}, err
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	r.c.call(closeCtx, "", "Browser.close", nil, nil)
	cancel()
	return res, nil
}

func (r *run) page(ctx context.Context, method string, params, out any) error {
	return r.c.call(ctx, r.session, method, params, out)
}

func (r *run) draw(ctx context.Context) (Result, error) {
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := r.c.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return Result{}, err
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := r.c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &att); err != nil {
		return Result{}, err
	}
	r.session = att.SessionID
	for _, m := range []string{"Page.enable", "Runtime.enable", "Log.enable"} {
		if err := r.page(ctx, m, nil, nil); err != nil {
			return Result{}, err
		}
	}
	if err := r.page(ctx, "Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*"}}}, nil); err != nil {
		return Result{}, err
	}
	w, h := r.req.Width, r.req.Height
	fixed := h != 0
	if !fixed {
		h = StartHeight
	}
	if err := r.viewport(ctx, w, h); err != nil {
		return Result{}, err
	}
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := r.page(ctx, "Page.navigate", map[string]any{"url": r.req.URL}, &nav); err != nil {
		return Result{}, err
	}
	if nav.ErrorText != "" {
		return Result{}, fmt.Errorf("the artifact didn't open: %s", nav.ErrorText)
	}
	select {
	case <-r.loaded:
	case <-time.After(20 * time.Second):
		r.note("the page was still loading after 20 seconds; this picture is what it showed then")
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	r.settle(ctx, true)

	res := Result{Width: w}
	if fixed {
		// The artifact's own height: the frame is exactly this tall, and
		// taller content scrolls inside it, as in a window of this size.
		content, _ := r.measure(ctx)
		y := r.req.Y
		if y > 0 {
			var sy float64
			r.eval(ctx, fmt.Sprintf("window.scrollTo(0, %d), window.scrollY", y), &sy)
			r.settle(ctx, false)
			y = int(sy)
		}
		png, err := r.capture(ctx, nil)
		if err != nil {
			return Result{}, err
		}
		res.PNG, res.Height, res.ContentHeight, res.ShotY, res.ShotHeight = png, h, content, y, h
	} else {
		// The frame follows the content, as Artifex's frames do.
		growth := 0
		for i := 0; i < 12; i++ {
			m, err := r.measure(ctx)
			if err != nil {
				return Result{}, err
			}
			m = max(50, min(m, MaxFrameHeight))
			if m == h {
				break
			}
			if m > h {
				if growth++; growth >= 6 {
					h, res.Locked = max(h, LockHeight), true
					if err := r.viewport(ctx, w, h); err != nil {
						return Result{}, err
					}
					r.settle(ctx, false)
					break
				}
			}
			h = m
			if err := r.viewport(ctx, w, h); err != nil {
				return Result{}, err
			}
			r.settle(ctx, false)
		}
		y := r.req.Y
		if y >= h {
			return Result{}, &Invalid{fmt.Sprintf("y is past the end of the artifact, which is %d pixels tall", h)}
		}
		piece := min(PieceHeight, h-y)
		png, err := r.capture(ctx, map[string]any{"x": 0, "y": y, "width": w, "height": piece, "scale": 1})
		if err != nil {
			return Result{}, err
		}
		res.PNG, res.Height, res.ContentHeight, res.ShotY, res.ShotHeight = png, h, h, y, piece
	}
	r.mu.Lock()
	res.Blocked, res.Missing, res.Errors = sortedKeys(r.blocked), sortedKeys(r.missing), r.errs
	r.mu.Unlock()
	return res, nil
}

func (r *run) viewport(ctx context.Context, w, h int) error {
	return r.page(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false}, nil)
}

// measureJS is the bridge's own measure (static/bridge.js, measure), so a
// frame here is as tall as it is in Artifex.
const measureJS = `(() => {
  const d = document.documentElement, b = document.body;
  if (!d) return 0;
  if (!b) return d.scrollHeight;
  const cs = getComputedStyle(b);
  const margins = (parseFloat(cs.marginTop) || 0) + (parseFloat(cs.marginBottom) || 0);
  const bottom = b.getBoundingClientRect().bottom + window.scrollY + (parseFloat(cs.marginBottom) || 0);
  return Math.ceil(Math.max(b.scrollHeight + margins, bottom));
})()`

func (r *run) measure(ctx context.Context) (int, error) {
	var h float64
	if err := r.eval(ctx, measureJS, &h); err != nil {
		return 0, err
	}
	return int(h), nil
}

func (r *run) eval(ctx context.Context, expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := r.page(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &res); err != nil {
		return err
	}
	if res.Exception != nil {
		return errors.New(res.Exception.Text)
	}
	if out != nil && len(res.Result.Value) > 0 {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}

// settle waits until the page has loaded what it asked for, its fonts are
// ready and it has drawn twice: then it shows what a person would see.
func (r *run) settle(ctx context.Context, first bool) {
	quiet := 150 * time.Millisecond
	if first {
		quiet = 300 * time.Millisecond
	}
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) && ctx.Err() == nil {
		if r.pending.Load() == 0 && time.Since(time.UnixMilli(r.last.Load())) >= quiet {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.eval(ctx, `document.fonts ? document.fonts.ready.then(() => 1) : 1`, nil)
	r.eval(ctx, `new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(1))))`, nil)
}

func (r *run) capture(ctx context.Context, clip map[string]any) ([]byte, error) {
	params := map[string]any{"format": "png", "fromSurface": true, "captureBeyondViewport": false}
	if clip != nil {
		params["clip"] = clip
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := r.page(ctx, "Page.captureScreenshot", params, &shot); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(shot.Data)
}

func (r *run) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs) < 50 {
		r.errs = append(r.errs, s)
	}
}

// quoted is the address in the browser's message for a load the content
// policy blocked: "Loading the script 'https://…' violates the following
// Content Security Policy directive …" (before Chromium 135 or so: "Refused
// to load the script 'https://…' because it violates …").
var quoted = regexp.MustCompile(`'([^']+)'`)

func (r *run) event(m message) {
	switch m.Method {
	case "Page.loadEventFired":
		select {
		case r.loaded <- struct{}{}:
		default:
		}
	case "Fetch.requestPaused":
		var p pausedRequest
		if json.Unmarshal(m.Params, &p) == nil {
			r.pending.Add(1)
			go r.serve(p)
		}
	case "Runtime.exceptionThrown":
		var p struct {
			Details struct {
				Text      string `json:"text"`
				URL       string `json:"url"`
				Line      int    `json:"lineNumber"`
				Exception *struct {
					Description string `json:"description"`
				} `json:"exception"`
			} `json:"exceptionDetails"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			d := p.Details
			text := d.Text
			if d.Exception != nil && d.Exception.Description != "" {
				text = firstLine(d.Exception.Description)
			}
			if d.URL != "" {
				text += fmt.Sprintf(" (%s:%d)", r.local(d.URL), d.Line+1)
			}
			r.note("script error: " + text)
		}
	case "Runtime.consoleAPICalled":
		var p struct {
			Type string `json:"type"`
			Args []struct {
				Value       any    `json:"value"`
				Description string `json:"description"`
			} `json:"args"`
		}
		if json.Unmarshal(m.Params, &p) == nil && (p.Type == "error" || p.Type == "assert") {
			var parts []string
			for _, a := range p.Args {
				if a.Value != nil {
					parts = append(parts, fmt.Sprint(a.Value))
				} else {
					parts = append(parts, firstLine(a.Description))
				}
			}
			r.note("console.error: " + strings.Join(parts, " "))
		}
	case "Log.entryAdded":
		var p struct {
			Entry struct {
				Source string `json:"source"`
				Level  string `json:"level"`
				Text   string `json:"text"`
			} `json:"entry"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.Entry.Level != "error" {
			return
		}
		if strings.Contains(p.Entry.Text, "Content Security Policy") {
			if sm := quoted.FindStringSubmatch(p.Entry.Text); sm != nil {
				r.mu.Lock()
				r.blocked[sm[1]] = true
				r.mu.Unlock()
				return
			}
		}
		if p.Entry.Source == "network" {
			return // failed loads: serve records them more precisely
		}
		r.note(firstLine(p.Entry.Text))
	}
}

type pausedRequest struct {
	RequestID string `json:"requestId"`
	Request   struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	} `json:"request"`
}

// serve answers one request of the page: Artifex's own files from Upstream,
// anything else refused.
func (r *run) serve(p pausedRequest) {
	defer func() {
		r.last.Store(time.Now().UnixMilli())
		r.pending.Add(-1)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, err := url.Parse(p.Request.URL)
	if err != nil || u.Scheme+"://"+u.Host != r.origin || (p.Request.Method != http.MethodGet && p.Request.Method != http.MethodHead) {
		if err == nil && u.Scheme+"://"+u.Host != r.origin {
			r.mu.Lock()
			r.blocked[p.Request.URL] = true
			r.mu.Unlock()
		}
		r.page(ctx, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"}, nil)
		return
	}
	rec := httptest.NewRecorder()
	if u.Path == "/favicon.ico" {
		// Asked for because the artifact is the top page here; a frame in
		// Artifex never asks.
		r.page(ctx, "Fetch.fulfillRequest", map[string]any{"requestId": p.RequestID, "responseCode": http.StatusNotFound}, nil)
		return
	}
	if strings.HasPrefix(u.Path, "/c/") || strings.HasPrefix(u.Path, "/static/") {
		req, _ := http.NewRequestWithContext(ctx, p.Request.Method, u.RequestURI(), nil)
		req.Host = u.Host
		r.b.Upstream.ServeHTTP(rec, req)
	} else {
		http.NotFound(rec, nil)
	}
	if rec.Code == http.StatusNotFound {
		r.mu.Lock()
		r.missing[r.local(p.Request.URL)] = true
		r.mu.Unlock()
	}
	var headers []map[string]string
	for k, vs := range rec.Header() {
		for _, v := range vs {
			headers = append(headers, map[string]string{"name": k, "value": v})
		}
	}
	r.page(ctx, "Fetch.fulfillRequest", map[string]any{
		"requestId": p.RequestID, "responseCode": rec.Code, "responseHeaders": headers,
		"body": base64.StdEncoding.EncodeToString(rec.Body.Bytes()),
	}, nil)
}

// local names a URL of the artifact's version by its path in the project.
func (r *run) local(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme+"://"+u.Host != r.origin {
		return raw
	}
	if r.prefix != "" && strings.HasPrefix(u.Path, r.prefix) {
		return strings.TrimPrefix(u.Path, r.prefix)
	}
	return u.Path + " (outside the project: a path starting with / points at the Artifex server)"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tail keeps the end of the browser's error output, for failures.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
