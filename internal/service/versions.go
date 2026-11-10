package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/store"
)

// FileContent is one file at a version.
type FileContent struct {
	Project       string `json:"project"`
	Version       string `json:"version"`
	Path          string `json:"path"`
	Blob          string `json:"blob"` // pass as base to change this file
	Size          int64  `json:"size"`
	Content       string `json:"content,omitempty"`
	ContentBase64 string `json:"content_base64,omitempty"`
}

// ReadFile returns a file's content at a version (default: the latest).
func (s *Service) ReadFile(ctx context.Context, a Actor, project, p, version string) (FileContent, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		return FileContent{}, err
	}
	p, err := CleanPath(p)
	if err != nil {
		return FileContent{}, err
	}
	commit, err := s.ResolveVersion(ctx, project, version)
	if err != nil {
		return FileContent{}, err
	}
	if commit == "" {
		return FileContent{}, notFound("project %q has no files yet", project)
	}
	e, err := s.Entry(ctx, project, commit, p)
	if err != nil {
		return FileContent{}, err
	}
	if e.Type != "blob" {
		return FileContent{}, invalid("%q is a folder; use list_files", p)
	}
	if e.Size > s.cfg.Limits.MaxFileBytes {
		return FileContent{}, invalid("%q is %d bytes, more than can be returned", p, e.Size)
	}
	data, err := s.Repo(project).ReadBlob(ctx, e.ID)
	if err != nil {
		return FileContent{}, err
	}
	fc := FileContent{Project: project, Version: commit, Path: p, Blob: e.ID, Size: e.Size}
	if utf8.Valid(data) {
		fc.Content = string(data)
	} else {
		fc.ContentBase64 = base64.StdEncoding.EncodeToString(data)
	}
	return fc, nil
}

// Version is one commit as people and agents see it.
type Version struct {
	ID      string    `json:"version"`
	Short   string    `json:"short"`
	Author  string    `json:"author"`
	Email   string    `json:"email"`
	When    time.Time `json:"when"`
	Message string    `json:"message"`
	Body    string    `json:"body,omitempty"`
}

func versionOf(c gitrepo.Commit) Version {
	return Version{ID: c.ID, Short: short(c.ID), Author: c.AuthorName, Email: c.AuthorEmail, When: c.When, Message: c.Subject, Body: c.Body}
}

// History lists a project's versions, or the versions that changed one file
// (following renames), newest first (§6.1).
func (s *Service) History(ctx context.Context, a Actor, project, p string, limit, skip int) ([]Version, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		return nil, err
	}
	if p != "" {
		var err error
		if p, err = CleanPath(p); err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	repo := s.Repo(project)
	head, err := repo.Head(ctx)
	if err != nil {
		return nil, err
	}
	commits, err := repo.Log(ctx, head, p, limit, skip)
	if err != nil {
		return nil, err
	}
	out := make([]Version, len(commits))
	for i, c := range commits {
		out[i] = versionOf(c)
	}
	return out, nil
}

// VersionInfo returns one version.
func (s *Service) VersionInfo(ctx context.Context, project, commit string) (Version, error) {
	c, err := s.Repo(project).CommitInfo(ctx, commit)
	if err != nil {
		return Version{}, err
	}
	return versionOf(c), nil
}

const maxDiff = 2 << 20

// Diff returns the source diff between two versions; to defaults to the
// latest, from to to's parent.
func (s *Service) Diff(ctx context.Context, a Actor, project, from, to, p string) (string, string, string, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		return "", "", "", err
	}
	if p != "" {
		var err error
		if p, err = CleanPath(p); err != nil {
			return "", "", "", err
		}
	}
	toC, err := s.ResolveVersion(ctx, project, to)
	if err != nil {
		return "", "", "", err
	}
	if toC == "" {
		return "", "", "", notFound("project %q has no versions yet", project)
	}
	var fromC string
	if from != "" {
		if fromC, err = s.ResolveVersion(ctx, project, from); err != nil {
			return "", "", "", err
		}
	} else if c, err := s.Repo(project).CommitInfo(ctx, toC); err == nil && len(c.Parents) > 0 {
		fromC = c.Parents[0]
	}
	d, err := s.Repo(project).Diff(ctx, fromC, toC, p)
	if err != nil {
		return "", "", "", err
	}
	if len(d) > maxDiff {
		d = d[:maxDiff] + "\n… (diff cut at 2 MB)\n"
	}
	return fromC, toC, d, nil
}

// Restore publishes an earlier version's content as a new commit (§6.3):
// one file, one folder, or the whole project when p is "".
func (s *Service) Restore(ctx context.Context, a Actor, project, version, p, message string) (PublishResult, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectEditor); err != nil {
		return PublishResult{}, err
	}
	scope, err := CleanDir(p)
	if err != nil {
		return PublishResult{}, err
	}
	old, err := s.ResolveVersion(ctx, project, version)
	if err != nil {
		return PublishResult{}, err
	}
	if old == "" {
		return PublishResult{}, notFound("version %q not found", version)
	}
	unlock := s.Lock(project)
	defer unlock()
	repo := s.Repo(project)
	head, err := repo.Head(ctx)
	if err != nil {
		return PublishResult{}, err
	}
	blobsAt := func(commit string) (map[string]gitrepo.Entry, error) {
		out := map[string]gitrepo.Entry{}
		if scope != "" {
			e, err := repo.Lookup(ctx, commit, scope)
			if errors.Is(err, gitrepo.ErrNotFound) {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			if e.Type == "blob" {
				e.Path = scope
				out[scope] = e
				return out, nil
			}
		}
		items, err := repo.ListTree(ctx, commit, scope, true)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Type == "blob" {
				out[it.Path] = it
			}
		}
		return out, nil
	}
	want, err := blobsAt(old)
	if err != nil {
		return PublishResult{}, err
	}
	if len(want) == 0 && scope != "" {
		return PublishResult{}, notFound("%q doesn't exist at version %s", scope, short(old))
	}
	have, err := blobsAt(head)
	if err != nil {
		return PublishResult{}, err
	}
	var writes []gitrepo.FileWrite
	var plan []plannedChange
	for p, e := range want {
		if h, ok := have[p]; ok && h.ID == e.ID {
			continue
		}
		writes = append(writes, gitrepo.FileWrite{Path: p, BlobID: e.ID, Mode: e.Mode})
		plan = append(plan, plannedChange{src: p, dst: p})
	}
	for p := range have {
		if _, ok := want[p]; !ok {
			writes = append(writes, gitrepo.FileWrite{Path: p, Remove: true})
			plan = append(plan, plannedChange{src: p, dst: p, del: true})
		}
	}
	what := scope
	if what == "" {
		what = "the project"
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("Restore %s to %s", what, short(old))
	}
	if len(writes) == 0 {
		return s.result(ctx, project, head, nil, true), nil
	}
	now := s.now()
	commit, err := repo.CommitFiles(ctx, head, writes, a.signature(now), s.committer(now), message)
	if errors.Is(err, gitrepo.ErrNoChanges) {
		return s.result(ctx, project, head, nil, true), nil
	}
	if err != nil {
		return PublishResult{}, err
	}
	repo.SyncWorktree(ctx)
	res := s.result(ctx, project, commit, plan, false)
	res.Warnings = nil
	s.hub.Publish(events.Event{Type: "version", Project: project, Version: commit, Paths: changedPaths(plan)})
	s.log.Info("restored", zap.String("project", project), zap.String("to", short(old)), zap.String("scope", what), zap.String("by", a.Name()))
	return res, nil
}

// PathBefore returns where the file at p in commit to was in commit from,
// following a rename; "" when it didn't exist. Callers check permissions.
func (s *Service) PathBefore(ctx context.Context, project, from, to, p string) string {
	changes, err := s.Repo(project).Changes(ctx, from, to)
	if err != nil {
		return ""
	}
	for _, c := range changes {
		if c.Path == p {
			if c.Status == 'R' {
				return c.OldPath
			}
			if c.Status == 'A' {
				return ""
			}
			return p
		}
	}
	return p
}
