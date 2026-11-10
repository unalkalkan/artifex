// Package service is Artifex's core: projects, publishing, versions, members,
// threads, accounts and tokens, with every permission check. The MCP server,
// the web app and git over HTTPS all call it, so a rule lives in one place.
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/store"
)

// Kind classifies service errors, so callers can map them to HTTP statuses
// and MCP error results.
type Kind int

const (
	KindInvalid Kind = iota + 1
	KindNotFound
	KindForbidden
	KindConflict
)

// Error is a service error with a message meant for the caller.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func invalid(format string, a ...any) error {
	return &Error{Kind: KindInvalid, Msg: fmt.Sprintf(format, a...)}
}
func notFound(format string, a ...any) error {
	return &Error{Kind: KindNotFound, Msg: fmt.Sprintf(format, a...)}
}
func forbidden(format string, a ...any) error {
	return &Error{Kind: KindForbidden, Msg: fmt.Sprintf(format, a...)}
}
func conflict(format string, a ...any) error {
	return &Error{Kind: KindConflict, Msg: fmt.Sprintf(format, a...)}
}

// KindOf returns the error's kind, or 0 for unexpected errors.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	var c *ConflictError
	if errors.As(err, &c) {
		return KindConflict
	}
	return 0
}

// Actor is who is acting: an account, and the token's label when an agent
// acts with that account's token.
type Actor struct {
	Account    store.Account
	AgentLabel string
	TokenID    int64
}

// Name is how the actor appears in history and threads.
func (a Actor) Name() string {
	if a.AgentLabel != "" {
		return a.AgentLabel + " for " + a.Account.DisplayName()
	}
	return a.Account.DisplayName()
}

func (a Actor) signature(now time.Time) gitrepo.Signature {
	return gitrepo.Signature{Name: a.Name(), Email: a.Account.Email, When: now}
}

type Service struct {
	cfg   *config.Config
	store *store.Store
	hub   *events.Hub
	log   *zap.Logger
	now   func() time.Time

	locks sync.Map // project id -> *sync.Mutex

	metaMu sync.Mutex
	meta   map[string]any // blob id -> parsed metadata (immutable per blob)

	follow *gitrepo.FollowCache // what each commit did to paths, for threads
}

func New(cfg *config.Config, st *store.Store, hub *events.Hub, log *zap.Logger) *Service {
	return &Service{cfg: cfg, store: st, hub: hub, log: log, now: time.Now, meta: map[string]any{}, follow: gitrepo.NewFollowCache()}
}

func (s *Service) Store() *store.Store    { return s.store }
func (s *Service) Hub() *events.Hub       { return s.hub }
func (s *Service) Config() *config.Config { return s.cfg }

// Lock serialises writes to one project: publishes here and pushes over git.
// The branch's compare-and-swap (gitrepo.CommitFiles) is the guarantee; the
// lock only keeps writers from failing against each other.
func (s *Service) Lock(project string) func() {
	m, _ := s.locks.LoadOrStore(project, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Repo returns a project's repository. It does not check that it exists.
func (s *Service) Repo(project string) *gitrepo.Repo {
	return gitrepo.Open(filepath.Join(s.cfg.ProjectsDir(), project))
}

// Role returns the actor's role in the project. Admins act as Owners of
// every project (design question 16). A project the actor can't see is
// reported as not found, so its existence doesn't leak.
func (s *Service) Role(ctx context.Context, a Actor, project string) (string, error) {
	if !validProjectID(project) {
		return "", notFound("project %q not found", project)
	}
	if _, err := s.store.ProjectByID(ctx, project); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", notFound("project %q not found", project)
		}
		return "", err
	}
	if a.Account.IsAdmin() {
		return store.ProjectOwner, nil
	}
	role, err := s.store.MemberRole(ctx, project, a.Account.ID)
	if err != nil {
		return "", err
	}
	if role == "" {
		return "", notFound("project %q not found", project)
	}
	return role, nil
}

// Require checks that the actor has at least role min in the project.
func (s *Service) Require(ctx context.Context, a Actor, project, min string) (string, error) {
	role, err := s.Role(ctx, a, project)
	if err != nil {
		return "", err
	}
	if store.RoleRank(role) < store.RoleRank(min) {
		return role, forbidden("this needs the %s role in project %q; you are %s", min, project, role)
	}
	return role, nil
}

// AppURL is the address of a project, folder or artifact in the web app.
func (s *Service) AppURL(project, path, version string) string {
	u := s.cfg.BaseURL + "/p/" + url.PathEscape(project)
	if path != "" {
		u += "/" + EscapePath(path)
	}
	if version != "" {
		u += "?v=" + url.QueryEscape(version)
	}
	return u
}

// EscapePath escapes each segment of a slash-separated path.
func EscapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (s *Service) cached(id string) (any, bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	v, ok := s.meta[id]
	return v, ok
}

func (s *Service) cache(id string, v any) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if len(s.meta) > 20000 {
		s.meta = map[string]any{}
	}
	s.meta[id] = v
}

func short(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}
