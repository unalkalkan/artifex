// Package web is the browser side of Artifex: sign-in, the project, artifact
// and canvas views, history and comparison, members, tokens and account
// management, plus the JSON API and live events the pages' script uses.
// People view and comment here; they don't edit (design decision 2).
package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/content"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Outline icons from @tabler/icons, drawn inline by the icon template
// function (icons/README.md).
//
//go:embed icons/*.svg
var iconFS embed.FS

type Server struct {
	svc     *service.Service
	content *content.Server
	log     *zap.Logger
	key     []byte
	pages   map[string]*template.Template
	assets  map[string]string // static file -> content hash
	gzipped map[string][]byte // static text file -> its gzip, made once
	icons   map[string]string // icon name -> the SVG's inner markup
	limiter *limiter
	oauth   *googleAuth
}

// Options wires the handlers that live in other packages.
type Options struct {
	MCP http.Handler
	Git http.Handler
}

func New(svc *service.Service, key []byte, log *zap.Logger) (*Server, error) {
	s := &Server{svc: svc, log: log, key: key, assets: map[string]string{}, gzipped: map[string][]byte{}}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		name := strings.TrimPrefix(p, "static/")
		s.assets[name] = hex.EncodeToString(h[:6])
		// Tabler's stylesheet is 550 KB; compressed it is a seventh of that.
		if ext := path.Ext(name); (ext == ".css" || ext == ".js" || ext == ".svg") && len(b) > 1024 {
			var z bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&z, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			s.gzipped[name] = z.Bytes()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.icons, err = loadIcons(); err != nil {
		return nil, err
	}
	s.content = content.New(svc, key, svc.Config().BaseURL+s.asset("bridge.js"))
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	cfg := svc.Config()
	s.limiter = newLimiter(cfg.Limits.RequestsPerMinute, cfg.TrustProxy)
	if cfg.GoogleEnabled() {
		s.oauth = newGoogleAuth(cfg)
	}
	return s, nil
}

// Content is the content server, for tests and the git handler's links.
func (s *Server) Content() *content.Server { return s.content }

func (s *Server) asset(name string) string {
	return "/static/" + name + "?v=" + s.assets[name]
}

// Handler returns every route of the service.
func (s *Server) Handler(o Options) http.Handler {
	mux := http.NewServeMux()
	page := func(pattern string, h func(http.ResponseWriter, *http.Request, service.Actor)) {
		mux.Handle(pattern, s.appHeaders(s.signedIn(h)))
	}
	api := func(pattern string, h func(http.ResponseWriter, *http.Request, service.Actor)) {
		mux.Handle(pattern, s.limiter.wrap(s.signedInAPI(h)))
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /static/", http.HandlerFunc(s.static))
	mux.Handle("/c/", s.content)
	if o.MCP != nil {
		// A publish's files arrive as JSON, binary ones in base64 (4/3 larger).
		limit := s.svc.Config().Limits.MaxPublishBytes*4/3 + 1<<20
		mux.Handle("/mcp", s.limiter.wrap(http.MaxBytesHandler(o.MCP, limit)))
	}
	if o.Git != nil {
		mux.Handle("/git/", s.limiter.wrap(o.Git))
	}

	// Sign-in.
	mux.Handle("GET /login", s.appHeaders(http.HandlerFunc(s.loginPage)))
	mux.Handle("GET /auth/google/start", s.limiter.wrap(http.HandlerFunc(s.googleStart)))
	mux.Handle("GET /auth/google/callback", s.appHeaders(s.limiter.wrap(http.HandlerFunc(s.googleCallback))))
	mux.Handle("/auth/dev", s.appHeaders(s.limiter.wrap(http.HandlerFunc(s.devSignIn))))
	mux.Handle("POST /logout", http.HandlerFunc(s.logout))

	// Pages.
	page("GET /{$}", s.home)
	page("GET /tokens", s.tokensPage)
	page("POST /tokens", s.tokensPost)
	page("GET /admin/accounts", s.accountsPage)
	page("POST /admin/accounts", s.accountsPost)
	page("GET /p/{project}", s.projectPage)
	page("GET /p/{project}/-/history", s.historyPage)
	page("GET /p/{project}/-/compare", s.comparePage)
	page("GET /p/{project}/-/members", s.membersPage)
	page("POST /p/{project}/-/members", s.membersPost)
	page("POST /p/{project}/-/restore", s.restorePost)
	page("POST /p/{project}/-/delete", s.deletePost)
	page("GET /p/{project}/{path...}", s.viewPage)

	// The pages' script.
	api("GET /api/projects/{project}/threads", s.apiThreads)
	api("POST /api/projects/{project}/threads", s.apiCreateThread)
	api("POST /api/threads/{id}/{action}", s.apiThreadAction)
	api("GET /api/projects/{project}/frame", s.apiFrame)
	api("GET /api/projects/{project}/events", s.apiEvents)

	return s.recoverer(mux)
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	switch path.Ext(name) {
	case ".js":
		h.Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		h.Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		h.Set("Content-Type", "image/svg+xml")
	}
	// Artifacts run with no origin and load the bridge from here.
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("v") == s.assets[name] {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	etag := `"` + s.assets[name] + `"`
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if z, ok := s.gzipped[name]; ok {
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
			b = z
		}
	}
	w.Write(b)
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(enc) == "gzip" && strings.ReplaceAll(strings.TrimSpace(q), " ", "") != "q=0" {
			return true
		}
	}
	return false
}

// svgInner is the markup between an icon file's <svg ...> and </svg>.
var svgInner = regexp.MustCompile(`(?s)^\s*<svg[^>]*>(.*)</svg>\s*$`)

func loadIcons() (map[string]string, error) {
	icons := map[string]string{}
	entries, err := iconFS.ReadDir("icons")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := iconFS.ReadFile("icons/" + e.Name())
		if err != nil {
			return nil, err
		}
		m := svgInner.FindSubmatch(b)
		if m == nil {
			return nil, fmt.Errorf("icon %s: not an SVG", e.Name())
		}
		icons[strings.TrimSuffix(e.Name(), ".svg")] = strings.TrimSpace(string(m[1]))
	}
	return icons, nil
}

// icon draws a Tabler icon inline, sized and coloured by Tabler's .icon
// class: {{icon "folder"}}, or {{icon "folder" "text-secondary"}} for more
// classes. An unknown name fails the page, so a typo shows in the tests.
func (s *Server) icon(name string, class ...string) (template.HTML, error) {
	inner, ok := s.icons[name]
	if !ok {
		return "", fmt.Errorf("no icon %q (add it to internal/web/icons)", name)
	}
	cls := "icon"
	if len(class) > 0 {
		cls += " " + template.HTMLEscapeString(strings.Join(class, " "))
	}
	return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" class="` + cls + `" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">` + inner + `</svg>`), nil
}

// appHeaders sets the security headers of the app's own pages.
func (s *Server) appHeaders(next http.Handler) http.Handler {
	// Artifact files are served from this origin too (/c/), so 'self' alone
	// would let a published script run in an app page if HTML were ever
	// injected into one. Scripts and stylesheets come only from /static/.
	static := s.svc.Config().Origin() + "/static/"
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src " + static,
		"style-src " + static + " 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"frame-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", zap.Any("value", v), zap.String("path", r.URL.Path), zap.ByteString("stack", debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- templates ---

var funcs = template.FuncMap{
	"short": func(id string) string {
		if len(id) > 10 {
			return id[:10]
		}
		return id
	},
	"iso":    func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"esc":    service.EscapePath,
	"base":   func(p string) string { return path.Base(p) },
	"parent": service.Parent,
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"json": func(v any) (template.JS, error) {
		b, err := json.Marshal(v)
		return template.JS(b), err
	},
	"rank": store.RoleRank,
	"add":  func(a, b int) int { return a + b },
	"sub":  func(a, b int) int { return a - b },
	"dict": func(kv ...any) (map[string]any, error) {
		if len(kv)%2 != 0 {
			return nil, errors.New("dict needs key and value pairs")
		}
		m := make(map[string]any, len(kv)/2)
		for i := 0; i < len(kv); i += 2 {
			k, ok := kv[i].(string)
			if !ok {
				return nil, errors.New("dict keys must be strings")
			}
			m[k] = kv[i+1]
		}
		return m, nil
	},
	"crumbs": func(p string) []crumb {
		if p == "" {
			return nil
		}
		parts := strings.Split(p, "/")
		out := make([]crumb, len(parts))
		for i := range parts {
			out[i] = crumb{Name: parts[i], Path: strings.Join(parts[:i+1], "/")}
		}
		return out
	},
	// initials are an avatar's letters: "Dev Person" -> "DP", "ana@x.test" -> "A".
	"initials": func(name, email string) string {
		src := strings.TrimSpace(name)
		if src == "" {
			src, _, _ = strings.Cut(email, "@")
		}
		out := ""
		for _, w := range strings.Fields(src) {
			r := []rune(w)
			out += strings.ToUpper(string(r[0]))
			if len([]rune(out)) == 2 {
				break
			}
		}
		if out == "" {
			return "?"
		}
		return out
	},
	"size": func(n int64) string {
		switch {
		case n < 1024:
			return fmt.Sprintf("%d B", n)
		case n < 1<<20:
			return fmt.Sprintf("%.1f KB", float64(n)/1024)
		}
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	},
}

type crumb struct{ Name, Path string }

func (s *Server) parseTemplates() error {
	s.pages = map[string]*template.Template{}
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		return err
	}
	fm := template.FuncMap{"asset": s.asset, "icon": s.icon, "sandbox": func() string { return content.Sandbox }}
	for k, v := range funcs {
		fm[k] = v
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(fm).ParseFS(templateFS, "templates/layout.html", "templates/"+name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		s.pages[strings.TrimSuffix(name, ".html")] = t
	}
	return nil
}

// pageData is what every page template gets.
type pageData struct {
	Title   string
	Page    string // body data-page, for the script
	Account *store.Account
	CSRF    string
	Wide    bool
	Data    any
	Notice  string
	Error   string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, pd pageData) {
	if sess := sessionFrom(r.Context()); sess != nil {
		pd.Account = &sess.account
		pd.CSRF = sess.csrf
	}
	if pd.Page == "" {
		pd.Page = name
	}
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.Execute(w, pd); err != nil {
		s.log.Error("template", zap.String("name", name), zap.Error(err))
	}
}

// fail shows a service error as a page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := s.statusOf(err)
	s.render(w, r, status, "error", pageData{Title: http.StatusText(status), Error: msg})
}

func (s *Server) statusOf(err error) (int, string) {
	switch service.KindOf(err) {
	case service.KindInvalid:
		return http.StatusBadRequest, err.Error()
	case service.KindNotFound:
		return http.StatusNotFound, err.Error()
	case service.KindForbidden:
		return http.StatusForbidden, err.Error()
	case service.KindConflict:
		return http.StatusConflict, err.Error()
	}
	if errors.Is(err, context.Canceled) {
		return 499, "canceled"
	}
	s.log.Error("request failed", zap.Error(err))
	return http.StatusInternalServerError, "Something went wrong. The server log has the details."
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) jsonError(w http.ResponseWriter, err error) {
	status, msg := s.statusOf(err)
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// safeNext keeps redirects inside the app.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}
