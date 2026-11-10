package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/store"
)

// Anchor is where a thread is pinned (design decision 14). Every field is
// optional: a thread an agent starts may pin the whole artifact.
type Anchor struct {
	Selector string   `json:"selector,omitempty" jsonschema:"CSS selector path of the element the comment is about"`
	Text     string   `json:"text,omitempty" jsonschema:"the element's text, shortened"`
	X        *float64 `json:"x,omitempty" jsonschema:"x of the point, in the artifact's own pixels"`
	Y        *float64 `json:"y,omitempty" jsonschema:"y of the point, in the artifact's own pixels"`
	OX       *float64 `json:"ox,omitempty" jsonschema:"x offset of the point inside the element"`
	OY       *float64 `json:"oy,omitempty" jsonschema:"y offset of the point inside the element"`
	Width    *int     `json:"width,omitempty" jsonschema:"the width the artifact was shown at"`
}

// EntryView is one comment, resolve or reopen.
type EntryView struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Author    string    `json:"author"`
	Agent     bool      `json:"agent,omitempty"`
	Body      string    `json:"body,omitempty"`
	Version   string    `json:"version,omitempty"` // for resolves: the version with the change
	CreatedAt time.Time `json:"created_at"`
	Mine      bool      `json:"mine,omitempty"`
}

// ThreadView is a thread as people and agents see it.
type ThreadView struct {
	ID              int64       `json:"id"`
	Project         string      `json:"project"`
	Path            string      `json:"path"`                    // where the artifact is now
	OriginalPath    string      `json:"original_path,omitempty"` // where it was when the thread started, if moved since
	ArtifactDeleted bool        `json:"artifact_deleted,omitempty"`
	Version         string      `json:"version"` // the version the thread was made on
	State           string      `json:"state"`
	ResolvedVersion string      `json:"resolved_version,omitempty"`
	Anchor          Anchor      `json:"anchor"`
	Entries         []EntryView `json:"entries"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	URL             string      `json:"url"`
}

// ThreadDetail adds what an agent needs to act on a thread (§5.4).
type ThreadDetail struct {
	ThreadView
	// Excerpt is the artifact's HTML around the element, at the thread's version.
	Excerpt string `json:"excerpt,omitempty"`
	// ChangedSince is true when the artifact changed after the thread was made.
	ChangedSince bool `json:"changed_since"`
}

// ThreadInput starts a thread.
type ThreadInput struct {
	Path    string
	Version string
	Anchor  Anchor
	Body    string
}

const maxBody = 20000

// CreateThread starts a thread on an artifact at a version (default: the latest).
func (s *Service) CreateThread(ctx context.Context, a Actor, project string, in ThreadInput) (ThreadView, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		return ThreadView{}, err
	}
	p, err := CleanPath(in.Path)
	if err != nil {
		return ThreadView{}, err
	}
	body, err := cleanBody(in.Body)
	if err != nil {
		return ThreadView{}, err
	}
	commit, err := s.ResolveVersion(ctx, project, in.Version)
	if err != nil {
		return ThreadView{}, err
	}
	e, err := s.Entry(ctx, project, commit, p)
	if err != nil {
		return ThreadView{}, err
	}
	if e.Type != "blob" {
		return ThreadView{}, invalid("%q is a folder; threads are about files", p)
	}
	an := in.Anchor
	an.Selector = clip(an.Selector, 2000)
	an.Text = clip(strings.Join(strings.Fields(an.Text), " "), 300)
	t := store.Thread{ProjectID: project, Path: p, CommitID: commit, Selector: an.Selector, ElementText: an.Text,
		X: an.X, Y: an.Y, OX: an.OX, OY: an.OY, ViewWidth: an.Width}
	id, err := s.store.CreateThread(ctx, t, s.entry(a, store.EntryComment, body, ""))
	if err != nil {
		return ThreadView{}, err
	}
	s.hub.Publish(events.Event{Type: "thread", Project: project, Thread: id, Path: p})
	return s.threadView(ctx, a, id)
}

// ThreadQuery selects threads.
type ThreadQuery struct {
	Project string // "" for every project the actor belongs to
	Path    string // only threads on this artifact (where it is now)
	State   string // "open", "resolved" or "" for both
	Since   time.Time
	Limit   int
}

// ListThreads lists threads the actor can see, most recently changed first.
func (s *Service) ListThreads(ctx context.Context, a Actor, q ThreadQuery) ([]ThreadView, error) {
	var projects []string
	if q.Project != "" {
		if _, err := s.Require(ctx, a, q.Project, store.ProjectCommenter); err != nil {
			return nil, err
		}
		projects = []string{q.Project}
	} else {
		pms, err := s.store.ListProjects(ctx, a.Account.ID, false)
		if err != nil {
			return nil, err
		}
		for _, pm := range pms {
			projects = append(projects, pm.ID)
		}
	}
	if q.State != "" && q.State != store.ThreadOpen && q.State != store.ThreadResolved {
		return nil, invalid("state must be open or resolved")
	}
	var path string
	if q.Path != "" {
		var err error
		if path, err = CleanPath(q.Path); err != nil {
			return nil, err
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	fetch := limit
	if path != "" {
		fetch = 2000 // filtered below, after following moves
	}
	ts, err := s.store.ListThreads(ctx, store.ThreadFilter{Projects: projects, State: q.State, Since: q.Since, Limit: fetch})
	if err != nil {
		return nil, err
	}
	heads := map[string]string{}
	out := make([]ThreadView, 0, len(ts))
	for _, t := range ts {
		v := s.view(ctx, a, t, heads)
		if path != "" && v.Path != path && t.Path != path {
			continue
		}
		out = append(out, v)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// GetThread returns one thread with the HTML around its element.
func (s *Service) GetThread(ctx context.Context, a Actor, id int64) (ThreadDetail, error) {
	t, err := s.readableThread(ctx, a, id)
	if err != nil {
		return ThreadDetail{}, err
	}
	d := ThreadDetail{ThreadView: s.view(ctx, a, t, map[string]string{})}
	repo := s.Repo(t.ProjectID)
	if old, data, err := repo.ReadFile(ctx, t.CommitID, t.Path); err == nil {
		d.Excerpt = excerpt(string(data), t.ElementText, t.Selector)
		if !d.ArtifactDeleted {
			head, _ := repo.Head(ctx)
			if cur, err := repo.Lookup(ctx, head, d.Path); err == nil && cur.ID != old.ID {
				d.ChangedSince = true
			}
		} else {
			d.ChangedSince = true
		}
	}
	return d, nil
}

// Reply adds a comment to a thread.
func (s *Service) Reply(ctx context.Context, a Actor, id int64, body string) (ThreadView, error) {
	return s.addEntry(ctx, a, id, store.EntryComment, body, "", true)
}

// Resolve closes a thread, naming the version with the change when there is one.
func (s *Service) Resolve(ctx context.Context, a Actor, id int64, version, body string) (ThreadView, error) {
	return s.addEntry(ctx, a, id, store.EntryResolved, body, version, false)
}

// Reopen opens a resolved thread again.
func (s *Service) Reopen(ctx context.Context, a Actor, id int64, body string) (ThreadView, error) {
	return s.addEntry(ctx, a, id, store.EntryReopened, body, "", false)
}

func (s *Service) addEntry(ctx context.Context, a Actor, id int64, kind, body, version string, bodyRequired bool) (ThreadView, error) {
	t, err := s.readableThread(ctx, a, id)
	if err != nil {
		return ThreadView{}, err
	}
	if bodyRequired || strings.TrimSpace(body) != "" {
		if body, err = cleanBody(body); err != nil {
			return ThreadView{}, err
		}
	}
	switch kind {
	case store.EntryResolved:
		if t.State == store.ThreadResolved {
			return ThreadView{}, conflict("thread %d is already resolved", id)
		}
		if version != "" {
			c, err := s.ResolveVersion(ctx, t.ProjectID, version)
			if err != nil {
				return ThreadView{}, err
			}
			version = c
		}
	case store.EntryReopened:
		if t.State == store.ThreadOpen {
			return ThreadView{}, conflict("thread %d is already open", id)
		}
	}
	if err := s.store.AddEntry(ctx, s.entryFor(t.ID, a, kind, body, version)); err != nil {
		return ThreadView{}, err
	}
	s.hub.Publish(events.Event{Type: "thread", Project: t.ProjectID, Thread: t.ID, Path: t.Path})
	return s.threadView(ctx, a, id)
}

func (s *Service) readableThread(ctx context.Context, a Actor, id int64) (store.Thread, error) {
	t, err := s.store.ThreadByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return t, notFound("thread %d not found", id)
	} else if err != nil {
		return t, err
	}
	if _, err := s.Require(ctx, a, t.ProjectID, store.ProjectCommenter); err != nil {
		if KindOf(err) == KindNotFound {
			return t, notFound("thread %d not found", id)
		}
		return t, err
	}
	return t, nil
}

func (s *Service) threadView(ctx context.Context, a Actor, id int64) (ThreadView, error) {
	t, err := s.store.ThreadByID(ctx, id)
	if err != nil {
		return ThreadView{}, err
	}
	return s.view(ctx, a, t, map[string]string{}), nil
}

// view maps a stored thread to its view, following the artifact to where it
// is now (§5.3). heads caches each project's head across a listing.
func (s *Service) view(ctx context.Context, a Actor, t store.Thread, heads map[string]string) ThreadView {
	v := ThreadView{ID: t.ID, Project: t.ProjectID, Path: t.Path, Version: t.CommitID, State: t.State, ResolvedVersion: t.ResolvedCommit,
		Anchor:    Anchor{Selector: t.Selector, Text: t.ElementText, X: t.X, Y: t.Y, OX: t.OX, OY: t.OY, Width: t.ViewWidth},
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	head, ok := heads[t.ProjectID]
	if !ok {
		head, _ = s.Repo(t.ProjectID).Head(ctx)
		heads[t.ProjectID] = head
	}
	cur, err := s.followCached(ctx, t.ProjectID, t.CommitID, head, t.Path)
	if err == nil {
		if cur == "" {
			v.ArtifactDeleted = true
		} else if cur != t.Path {
			v.Path, v.OriginalPath = cur, t.Path
		}
	}
	v.URL = s.AppURL(t.ProjectID, v.Path, "") + "#t" + itoa(t.ID)
	for _, e := range t.Entries {
		v.Entries = append(v.Entries, EntryView{ID: e.ID, Kind: e.Kind, Author: e.Author(), Agent: e.AgentLabel != "", Body: e.Body,
			Version: e.CommitID, CreatedAt: e.CreatedAt, Mine: e.AccountID == a.Account.ID && e.AgentLabel == a.AgentLabel})
	}
	return v
}

func (s *Service) followCached(ctx context.Context, project, from, to, p string) (string, error) {
	key := "follow:" + project + ":" + from + ":" + to + ":" + p
	if v, ok := s.cached(key); ok {
		return v.(string), nil
	}
	cur, err := s.Repo(project).FollowPath(ctx, from, to, p, s.follow)
	if err == nil {
		s.cache(key, cur)
	}
	return cur, err
}

func (s *Service) entry(a Actor, kind, body, version string) store.ThreadEntry {
	return s.entryFor(0, a, kind, body, version)
}

func (s *Service) entryFor(thread int64, a Actor, kind, body, version string) store.ThreadEntry {
	return store.ThreadEntry{ThreadID: thread, Kind: kind, AccountID: a.Account.ID, AuthorName: a.Account.DisplayName(),
		AgentLabel: a.AgentLabel, Body: body, CommitID: version}
}

func cleanBody(b string) (string, error) {
	b = strings.TrimSpace(b)
	if b == "" {
		return "", invalid("the comment is empty")
	}
	if utf8.RuneCountInString(b) > maxBody {
		return "", invalid("the comment is longer than %d characters", maxBody)
	}
	return b, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// excerpt returns the HTML around the element a thread points at, found by
// its text or its id.
func excerpt(src, text, selector string) string {
	at := -1
	if t := strings.TrimSpace(text); t != "" {
		words := strings.Fields(t)
		for n := len(words); n > 0 && at < 0; n-- {
			probe := strings.Join(words[:n], " ")
			if len(probe) < 3 {
				break
			}
			at = strings.Index(src, probe)
		}
	}
	if at < 0 {
		if i := strings.LastIndex(selector, "#"); i >= 0 {
			id := selector[i+1:]
			if j := strings.IndexAny(id, " >.:["); j >= 0 {
				id = id[:j]
			}
			if id != "" {
				at = strings.Index(src, `id="`+id+`"`)
			}
		}
	}
	if at < 0 {
		return ""
	}
	start, end := at-700, at+900
	if start < 0 {
		start = 0
	}
	if end > len(src) {
		end = len(src)
	}
	for start > 0 && !utf8.RuneStart(src[start]) {
		start--
	}
	for end < len(src) && !utf8.RuneStart(src[end]) {
		end++
	}
	out := src[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(src) {
		out += "…"
	}
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
