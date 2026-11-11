// Package content serves project files to the browser at signed addresses,
// isolated from the app (design decisions 10 and 11):
//
//	/c/<expiry>.<signature>/<project>/<commit>/<path>
//
// The signature covers the expiry, the project and the commit, not the path,
// so an artifact's relative links to other files of the same version resolve
// under the same prefix and are signed too. Every response carries a CSP with
// `sandbox`, so even when opened directly in a tab an artifact runs without
// an origin and can't act as the viewer.
package content

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/service"
)

// Sandbox is the iframe sandbox and the CSP sandbox of every artifact
// (design decision 10). No allow-same-origin: the artifact gets an opaque
// origin. Popups may leave the sandbox so an external link opens as a normal
// page; Artifex content opened that way is sandboxed again by its CSP. No
// modals (prompt() could imitate a sign-in dialog) and no downloads (they
// would come from the Artifex host).
const Sandbox = "allow-scripts allow-popups allow-popups-to-escape-sandbox"

type Server struct {
	svc    *service.Service
	key    []byte
	origin string
	ttl    time.Duration
	bridge []byte // the bridge as an inline <script> element
	tagVer string // a short hash of the bridge, part of an HTML file's ETag
	csp    string

	mu    sync.Mutex
	blobs map[string][]byte // blob id -> content, bounded
	size  int
}

// New makes the content server. bridgeJS is the bridge's source (design §4.4), which every HTML file gets
// inline.
func New(svc *service.Service, key []byte, bridgeJS []byte) *Server {
	cfg := svc.Config()
	o := cfg.Origin()
	src := "'self' " + o + " data: blob:"
	csp := strings.Join([]string{
		"default-src " + src,
		"script-src " + src + " 'unsafe-inline' 'unsafe-eval'",
		"style-src " + src + " 'unsafe-inline'",
		"img-src " + src,
		"font-src " + src,
		"media-src " + src,
		"connect-src " + src,
		"worker-src " + src,
		"frame-src " + src,
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors " + o,
		"sandbox " + Sandbox,
	}, "; ")
	// "</script" inside the source would end the element early; in JavaScript "<\/" reads as "</".
	js := bytes.ReplaceAll(bridgeJS, []byte("</"), []byte(`<\/`))
	tag := append(append([]byte("<script>"), js...), "</script>"...)
	sum := sha256.Sum256(bridgeJS)
	return &Server{svc: svc, key: key, origin: o, ttl: cfg.Limits.ContentURLTTL, bridge: tag,
		tagVer: base64.RawURLEncoding.EncodeToString(sum[:6]), csp: csp, blobs: map[string][]byte{}}
}

// CSP is the Content-Security-Policy every content response carries.
func (s *Server) CSP() string { return s.csp }

// URL returns a signed address for a file of a project at a commit.
func (s *Server) URL(project, commit, p string) string {
	return s.Prefix(project, commit) + service.EscapePath(p)
}

// Prefix returns the signed folder address of a project at a commit, ending in "/".
func (s *Server) Prefix(project, commit string) string {
	exp := time.Now().Add(s.ttl).Unix()
	return s.origin + "/c/" + s.token(exp, project, commit) + "/" + project + "/" + commit + "/"
}

func (s *Server) token(exp int64, project, commit string) string {
	e := strconv.FormatInt(exp, 36)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("content\x00" + e + "\x00" + project + "\x00" + commit))
	return e + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

func (s *Server) verify(tok, project, commit string) error {
	e, _, ok := strings.Cut(tok, ".")
	if !ok {
		return errors.New("malformed")
	}
	exp, err := strconv.ParseInt(e, 36, 64)
	if err != nil {
		return errors.New("malformed")
	}
	if !hmac.Equal([]byte(tok), []byte(s.token(exp, project, commit))) {
		return errors.New("bad signature")
	}
	if time.Now().Unix() > exp {
		return errors.New("expired")
	}
	return nil
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ServeHTTP serves /c/<token>/<project>/<commit>/<path>.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/c/"), "/", 4)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	tok, project, commit := parts[0], parts[1], parts[2]
	p := ""
	if len(parts) == 4 {
		p = parts[3]
	}
	h := w.Header()
	h.Set("Content-Security-Policy", s.csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	if !commitPattern.MatchString(commit) {
		http.NotFound(w, r)
		return
	}
	if err := s.verify(tok, project, commit); err != nil {
		h.Set("Cache-Control", "no-store")
		http.Error(w, "This address has expired or is not valid. Reload the page in Artifex.", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	if p = strings.TrimSuffix(p, "/"); p != "" {
		clean, err := service.CleanPath(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		p = clean
	}
	repo := s.svc.Repo(project)
	e, err := repo.Lookup(ctx, commit, p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if e.Type == "tree" {
		// A folder serves its index.html.
		if !strings.HasSuffix(r.URL.Path, "/") && p != "" {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		p = path.Join(p, "index.html")
		if e, err = repo.Lookup(ctx, commit, p); err != nil || e.Type != "blob" {
			http.NotFound(w, r)
			return
		}
	}
	h.Set("ETag", `"`+e.ID+`"`)
	// The address names one commit, so the content never changes.
	h.Set("Cache-Control", "private, max-age=3600, immutable")
	if e.Size > streamAbove && !service.IsHTML(p) {
		// Large files go straight from git to the client, never whole in memory.
		h.Set("Content-Type", contentType(p, nil))
		h.Set("Content-Length", strconv.FormatInt(e.Size, 10))
		if r.Method == http.MethodHead {
			return
		}
		repo.StreamBlob(ctx, e.ID, w)
		return
	}
	data, err := s.blob(ctx, repo, e.ID)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	ctype := contentType(p, data)
	if strings.HasPrefix(ctype, "text/html") {
		data = s.inject(data)
		// The page carries the bridge too, so a new Artifex release is a new page.
		h.Set("ETag", `"`+e.ID+"-"+s.tagVer+`"`)
	}
	h.Set("Content-Type", ctype)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

func (s *Server) blob(ctx context.Context, repo *gitrepo.Repo, id string) ([]byte, error) {
	s.mu.Lock()
	if b, ok := s.blobs[id]; ok {
		s.mu.Unlock()
		return b, nil
	}
	s.mu.Unlock()
	b, err := repo.ReadBlob(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(b) <= 2<<20 {
		s.mu.Lock()
		if s.size+len(b) > 128<<20 {
			s.blobs, s.size = map[string][]byte{}, 0
		}
		s.blobs[id] = b
		s.size += len(b)
		s.mu.Unlock()
	}
	return b, nil
}

// streamAbove is the size above which non-HTML files are streamed.
const streamAbove = 2 << 20

var headOpen = regexp.MustCompile(`(?i)<head(\s[^>]*)?>`)

// inject adds the bridge script (design §4.4) at the start of <head>, so it
// runs before the artifact's own scripts. It goes in inline: as a separate
// file it would be one more request that holds up the artifact's first paint,
// on every view, because browsers don't cache what an iframe without an
// origin loads. The file in the repository is never changed.
func (s *Server) inject(data []byte) []byte {
	tag := s.bridge
	if loc := headOpen.FindIndex(data); loc != nil {
		out := make([]byte, 0, len(data)+len(tag))
		out = append(out, data[:loc[1]]...)
		out = append(out, tag...)
		return append(out, data[loc[1]:]...)
	}
	// No <head>: put it after the doctype, if any, so the document stays in
	// standards mode.
	if i := bytes.Index(bytes.ToLower(data[:min(len(data), 512)]), []byte("<!doctype")); i >= 0 {
		if j := bytes.IndexByte(data[i:], '>'); j >= 0 {
			at := i + j + 1
			out := make([]byte, 0, len(data)+len(tag))
			out = append(out, data[:at]...)
			out = append(out, tag...)
			return append(out, data[at:]...)
		}
	}
	return append(tag, data...)
}

func contentType(p string, data []byte) string {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".md", ".txt", ".csv", ".yaml", ".yml":
		return "text/plain; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".wasm":
		return "application/wasm"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	if data == nil {
		return "application/octet-stream"
	}
	return http.DetectContentType(data)
}
