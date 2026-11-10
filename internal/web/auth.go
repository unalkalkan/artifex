package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

const (
	sessionCookie = "afx_session"
	oauthCookie   = "afx_oauth"
)

type session struct {
	account store.Account
	csrf    string
}

type ctxKey struct{}

func sessionFrom(ctx context.Context) *session {
	s, _ := ctx.Value(ctxKey{}).(*session)
	return s
}

func (s *Server) loadSession(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, acct, err := s.svc.Store().SessionByHash(r.Context(), service.HashSecret(c.Value))
	if err != nil || !acct.IsActive() {
		return nil
	}
	return &session{account: acct, csrf: sess.CSRF}
}

// signedIn sends people without a session to the sign-in page, and checks
// the CSRF token and the Origin on every form post.
func (s *Server) signedIn(h func(http.ResponseWriter, *http.Request, service.Actor)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.loadSession(r)
		if sess == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Error(w, "sign in first", http.StatusUnauthorized)
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) || !tokenEqual(r.FormValue("csrf"), sess.csrf) {
				http.Error(w, "the form expired; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, sess)
		h(w, r.WithContext(ctx), service.Actor{Account: sess.account})
	})
}

// signedInAPI is signedIn for the pages' script: JSON errors, and the CSRF
// token in the X-CSRF-Token header.
func (s *Server) signedInAPI(h func(http.ResponseWriter, *http.Request, service.Actor)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.loadSession(r)
		if sess == nil {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) || !tokenEqual(r.Header.Get("X-CSRF-Token"), sess.csrf) {
				s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "the page expired; reload it"})
				return
			}
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, sess)
		h(w, r.WithContext(ctx), service.Actor{Account: sess.account})
	})
}

func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		// Older browsers send no Origin on same-origin form posts; fall back
		// to the Referer, and to the CSRF token alone when both are missing.
		ref := r.Header.Get("Referer")
		if ref == "" {
			return true
		}
		u, err := url.Parse(ref)
		return err == nil && u.Scheme+"://"+u.Host == s.svc.Config().Origin()
	}
	return o == s.svc.Config().Origin()
}

func tokenEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, acct store.Account) error {
	id := randomToken(32)
	ttl := s.svc.Config().Limits.SessionTTL
	if err := s.svc.Store().CreateSession(r.Context(), service.HashSecret(id), acct.ID, randomToken(24), time.Now().Add(ttl)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: s.svc.Config().Secure(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())})
	return nil
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.loadSession(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	cfg := s.svc.Config()
	s.render(w, r, http.StatusOK, "login", pageData{Title: "Sign in", Data: map[string]any{
		"Next": safeNext(r.URL.Query().Get("next")), "Google": s.oauth != nil, "Dev": cfg.DevAuth,
	}})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		sess := s.loadSession(r)
		if sess != nil && !tokenEqual(r.FormValue("csrf"), sess.csrf) {
			http.Error(w, "the form expired; reload the page and try again", http.StatusForbidden)
			return
		}
		s.svc.Store().DeleteSession(r.Context(), service.HashSecret(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.svc.Config().Secure(), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// finishSignIn shows the waiting or disabled page, or starts a session.
func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request, acct store.Account, next string) {
	switch acct.Status {
	case store.StatusPending:
		s.render(w, r, http.StatusOK, "message", pageData{Title: "Waiting for an Admin",
			Notice: fmt.Sprintf("You're signed in as %s. An Admin has to verify your account before you can use Artifex. Try again once they have.", acct.Email)})
		return
	case store.StatusDisabled:
		s.render(w, r, http.StatusForbidden, "message", pageData{Title: "Account disabled", Notice: "Your account is disabled. Ask an Admin to enable it."})
		return
	}
	if err := s.startSession(w, r, acct); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// devSignIn signs anyone in by email, for local development (dev_auth).
func (s *Server) devSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.svc.Config().DevAuth {
		http.NotFound(w, r)
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if email == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = strings.Split(email, "@")[0]
	}
	acct, err := s.svc.SignIn(r.Context(), email, name, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.finishSignIn(w, r, acct, r.FormValue("next"))
}

// --- Google ---

type googleAuth struct {
	conf   *oauth2.Config
	cfg    *config.Config
	client *http.Client
}

func newGoogleAuth(cfg *config.Config) *googleAuth {
	return &googleAuth{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}, conf: &oauth2.Config{
		ClientID:     cfg.Google.ClientID,
		ClientSecret: cfg.Google.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL:  "https://oauth2.googleapis.com/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		RedirectURL: cfg.BaseURL + "/auth/google/callback",
		Scopes:      []string{"openid", "email", "profile"},
	}}
}

type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Next     string `json:"n"`
	Exp      int64  `json:"e"`
}

func (s *Server) sign(b []byte) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("oauth\x00"))
	m.Write(b)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	st := oauthState{State: randomToken(18), Verifier: oauth2.GenerateVerifier(), Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(10 * time.Minute).Unix()}
	b, _ := json.Marshal(st)
	val := base64.RawURLEncoding.EncodeToString(b) + "." + s.sign(b)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: val, Path: "/auth/google", HttpOnly: true, Secure: s.svc.Config().Secure(), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier), oauth2.SetAuthURLParam("prompt", "select_account")}
	if d := s.svc.Config().Google.AllowedDomains; len(d) == 1 {
		opts = append(opts, oauth2.SetAuthURLParam("hd", d[0]))
	}
	http.Redirect(w, r, s.oauth.conf.AuthCodeURL(st.State, opts...), http.StatusFound)
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	bad := func(msg string) {
		s.render(w, r, http.StatusBadRequest, "message", pageData{Title: "Sign-in failed", Notice: msg + " Go back and try again."})
	}
	c, err := r.Cookie(oauthCookie)
	if err != nil {
		bad("The sign-in took too long or cookies are blocked.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth/google", MaxAge: -1})
	raw, sig, _ := strings.Cut(c.Value, ".")
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || !hmac.Equal([]byte(sig), []byte(s.sign(b))) {
		bad("The sign-in state is not valid.")
		return
	}
	var st oauthState
	if json.Unmarshal(b, &st) != nil || time.Now().Unix() > st.Exp || !tokenEqual(st.State, r.URL.Query().Get("state")) {
		bad("The sign-in state is not valid or has expired.")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		bad("Google refused the sign-in (" + e + ").")
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, s.oauth.client)
	tok, err := s.oauth.conf.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.log.Warn("google exchange failed", zap.Error(err))
		bad("Google didn't accept the sign-in.")
		return
	}
	resp, err := s.oauth.conf.Client(ctx, tok).Get("https://openidconnect.googleapis.com/v1/userinfo")
	if err != nil {
		bad("Couldn't read your Google account.")
		return
	}
	defer resp.Body.Close()
	var info struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		HD            string `json:"hd"` // the Workspace domain; empty for personal accounts
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&info) != nil || info.Email == "" {
		bad("Couldn't read your Google account.")
		return
	}
	if !info.EmailVerified {
		bad("Your Google address is not verified.")
		return
	}
	// A personal Google account can carry a company address (made before the
	// company used Workspace), so the address alone proves nothing: with
	// allowed_domains set, Google's own Workspace domain claim must match too.
	if d := s.svc.Config().Google.AllowedDomains; len(d) > 0 && !s.svc.Config().DomainAllowed("x@"+strings.ToLower(info.HD)) {
		bad(fmt.Sprintf("%s is not a %s Workspace account.", info.Email, strings.Join(d, " or ")))
		return
	}
	if !s.svc.Config().DomainAllowed(info.Email) {
		bad(fmt.Sprintf("%s can't sign in here; use your company account.", info.Email))
		return
	}
	acct, err := s.svc.SignIn(r.Context(), info.Email, info.Name, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.finishSignIn(w, r, acct, st.Next)
}

// --- rate limits ---

type limiter struct {
	mu         sync.Mutex
	perMinute  int
	trustProxy bool
	clients    map[string]*clientLimit
	lastSweep  time.Time
}

type clientLimit struct {
	l    *rate.Limiter
	seen time.Time
}

func newLimiter(perMinute int, trustProxy bool) *limiter {
	return &limiter{perMinute: perMinute, trustProxy: trustProxy, clients: map[string]*clientLimit{}, lastSweep: time.Now()}
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.lastSweep) > 10*time.Minute {
		for k, c := range l.clients {
			if now.Sub(c.seen) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.lastSweep = now
	}
	c, ok := l.clients[ip]
	if !ok {
		c = &clientLimit{l: rate.NewLimiter(rate.Limit(float64(l.perMinute)/60), max(l.perMinute/4, 10))}
		l.clients[ip] = c
	}
	c.seen = now
	return c.l.Allow()
}

func (l *limiter) clientIP(r *http.Request) string {
	if l.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *limiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(l.clientIP(r)) {
			w.Header().Set("Retry-After", "10")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
