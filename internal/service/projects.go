package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/store"
)

// ProjectView describes a project to the people and agents who can see it.
type ProjectView struct {
	ID          string    `json:"project"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Open        string    `json:"open,omitempty"` // the artifact to open first
	Role        string    `json:"role"`
	Version     string    `json:"version,omitempty"` // head; empty for a project with no commit yet
	UpdatedAt   time.Time `json:"updated_at"`
	URL         string    `json:"url"`
}

// ProjectMeta is project.yaml (design decision 12). Every field is optional.
type ProjectMeta struct {
	Title       string `yaml:"title"`
	Description string `yaml:"description"`
	Open        string `yaml:"open"`
	// Order orders the project's top folder, as folder.yaml's order does
	// for its folder. It is used over a top-level folder.yaml's.
	Order []string `yaml:"order"`
}

// FolderMeta is folder.yaml (design decision 12). Every field is optional.
type FolderMeta struct {
	Title string `yaml:"title"`
	Label string `yaml:"label"`
	// Order names the folder's artifacts and folders, in the order they are
	// shown. The rest follow by name.
	Order []string `yaml:"order"`
}

// ArtifactMeta is what an artifact says about itself.
type ArtifactMeta struct {
	Title  string
	Width  int // 0: fills the window
	Height int // 0: the frame follows the content
}

// CreateProject creates a project with a git repository; the actor becomes
// its Owner.
func (s *Service) CreateProject(ctx context.Context, a Actor, id, title string) (ProjectView, error) {
	if !validProjectID(id) {
		return ProjectView{}, invalid("project id %q must be 1–64 lowercase letters, digits and dashes, starting and ending with a letter or digit", id)
	}
	if _, err := s.store.ProjectByID(ctx, id); err == nil {
		return ProjectView{}, conflict("project id %q is taken", id)
	} else if !errors.Is(err, store.ErrNotFound) {
		return ProjectView{}, err
	}
	dir := filepath.Join(s.cfg.ProjectsDir(), id)
	if _, err := os.Stat(dir); err == nil {
		return ProjectView{}, conflict("project id %q is taken", id)
	}
	unlock := s.Lock(id)
	defer unlock()
	if err := s.store.CreateProject(ctx, id, a.Account.ID); err != nil {
		return ProjectView{}, err
	}
	repo, err := gitrepo.Init(ctx, dir)
	if err != nil {
		s.store.DeleteProject(ctx, id)
		os.RemoveAll(dir)
		return ProjectView{}, err
	}
	if t := strings.TrimSpace(title); t != "" {
		data, _ := yaml.Marshal(ProjectMeta{Title: t})
		now := s.now()
		if _, err := repo.CommitFiles(ctx, "", []gitrepo.FileWrite{{Path: "project.yaml", Data: data}}, a.signature(now), s.committer(now), "Create project "+id); err != nil {
			s.log.Warn("first commit failed", zapErr(err))
		} else {
			repo.SyncWorktree(ctx)
		}
	}
	s.log.Info("project created")
	return s.projectView(ctx, id, store.ProjectOwner)
}

// ListProjects lists the projects the actor belongs to. Admins also see the
// others, with their role empty there.
func (s *Service) ListProjects(ctx context.Context, a Actor) ([]ProjectView, error) {
	pms, err := s.store.ListProjects(ctx, a.Account.ID, a.Account.IsAdmin())
	if err != nil {
		return nil, err
	}
	out := make([]ProjectView, 0, len(pms))
	for _, pm := range pms {
		v, err := s.projectView(ctx, pm.ID, pm.Role)
		if err != nil {
			s.log.Warn("project unreadable", zapErr(err))
			continue
		}
		v.UpdatedAt = maxTime(v.UpdatedAt, pm.CreatedAt)
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Project returns one project the actor can see.
func (s *Service) Project(ctx context.Context, a Actor, id string) (ProjectView, error) {
	role, err := s.Role(ctx, a, id)
	if err != nil {
		return ProjectView{}, err
	}
	if a.Account.IsAdmin() {
		if r, _ := s.store.MemberRole(ctx, id, a.Account.ID); r != "" {
			role = r
		} else {
			role = "admin"
		}
	}
	return s.projectView(ctx, id, role)
}

func (s *Service) projectView(ctx context.Context, id, role string) (ProjectView, error) {
	v := ProjectView{ID: id, Title: id, Role: role, URL: s.AppURL(id, "", "")}
	repo := s.Repo(id)
	head, err := repo.Head(ctx)
	if err != nil {
		return v, err
	}
	v.Version = head
	if head == "" {
		if p, err := s.store.ProjectByID(ctx, id); err == nil {
			v.UpdatedAt = p.CreatedAt
		}
		return v, nil
	}
	if c, err := repo.CommitInfo(ctx, head); err == nil {
		v.UpdatedAt = c.When
	}
	m := s.projectMeta(ctx, repo, head)
	if m.Title != "" {
		v.Title = m.Title
	}
	v.Description = m.Description
	v.Open = m.Open
	return v, nil
}

func (s *Service) projectMeta(ctx context.Context, repo *gitrepo.Repo, commit string) ProjectMeta {
	e, err := repo.Lookup(ctx, commit, "project.yaml")
	if err != nil || e.Type != "blob" {
		return ProjectMeta{}
	}
	if v, ok := s.cached("p:" + e.ID); ok {
		return v.(ProjectMeta)
	}
	var m ProjectMeta
	if data, err := repo.ReadBlob(ctx, e.ID); err == nil {
		yaml.Unmarshal(data, &m)
	}
	if m.Open != "" {
		if p, err := CleanPath(m.Open); err == nil {
			m.Open = p
		} else {
			m.Open = ""
		}
	}
	s.cache("p:"+e.ID, m)
	return m
}

// DeleteProject moves the project's repository to the trash folder and
// forgets its members and threads. Owners only.
func (s *Service) DeleteProject(ctx context.Context, a Actor, id string) error {
	if _, err := s.Require(ctx, a, id, store.ProjectOwner); err != nil {
		return err
	}
	unlock := s.Lock(id)
	defer unlock()
	if err := os.MkdirAll(s.cfg.TrashDir(), 0o750); err != nil {
		return err
	}
	dst := filepath.Join(s.cfg.TrashDir(), fmt.Sprintf("%s-%d", id, s.now().Unix()))
	if err := os.Rename(filepath.Join(s.cfg.ProjectsDir(), id), dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.store.DeleteProject(ctx, id)
}

// ResolveVersion turns a version string into a commit of the project.
func (s *Service) ResolveVersion(ctx context.Context, project, version string) (string, error) {
	c, err := s.Repo(project).ResolveCommit(ctx, version)
	if errors.Is(err, gitrepo.ErrBadRevision) {
		return "", notFound("version %q not found in project %q", version, project)
	}
	return c, err
}

// FileInfo is one file or folder at a version.
type FileInfo struct {
	Path string `json:"path"`
	Type string `json:"type"` // "file" or "folder"
	Blob string `json:"blob,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// ListFiles lists a folder at a version.
func (s *Service) ListFiles(ctx context.Context, a Actor, project, dir, version string, recursive bool) (string, []FileInfo, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		return "", nil, err
	}
	dir, err := CleanDir(dir)
	if err != nil {
		return "", nil, err
	}
	commit, err := s.ResolveVersion(ctx, project, version)
	if err != nil || commit == "" {
		return commit, nil, err
	}
	items, err := s.Repo(project).ListTree(ctx, commit, dir, recursive)
	if errors.Is(err, gitrepo.ErrNotFound) {
		return commit, nil, notFound("folder %q not found at version %s", dir, short(commit))
	}
	if err != nil {
		return commit, nil, err
	}
	out := make([]FileInfo, 0, len(items))
	for _, it := range items {
		fi := FileInfo{Path: it.Path, Type: "file", Blob: it.ID, Size: it.Size}
		if it.Type == "tree" {
			fi = FileInfo{Path: it.Path, Type: "folder"}
		}
		out = append(out, fi)
	}
	return commit, out, nil
}

// Entry looks up a path at a commit without permission checks; callers check.
func (s *Service) Entry(ctx context.Context, project, commit, p string) (gitrepo.Entry, error) {
	e, err := s.Repo(project).Lookup(ctx, commit, p)
	if errors.Is(err, gitrepo.ErrNotFound) {
		return e, notFound("%q not found at version %s", p, short(commit))
	}
	return e, err
}

// Folder is a folder of a project at a version, as the web app shows it.
type Folder struct {
	Path      string
	Meta      FolderMeta
	Folders   []FileInfo
	Artifacts []Artifact
	Files     []FileInfo // everything else
}

// Artifact is one HTML file in a folder.
type Artifact struct {
	Path   string
	Name   string
	Blob   string
	Title  string
	Width  int
	Height int
}

// FolderAt reads a folder for display. Callers check permissions.
func (s *Service) FolderAt(ctx context.Context, project, commit, dir string) (Folder, error) {
	f := Folder{Path: dir}
	repo := s.Repo(project)
	items, err := repo.ListTree(ctx, commit, dir, false)
	if err != nil {
		if errors.Is(err, gitrepo.ErrNotFound) {
			return f, notFound("folder %q not found", dir)
		}
		return f, err
	}
	for _, it := range items {
		switch {
		case it.Type == "tree":
			f.Folders = append(f.Folders, FileInfo{Path: it.Path, Type: "folder"})
		case it.Name == "folder.yaml":
			if v, ok := s.cached("f:" + it.ID); ok {
				f.Meta = v.(FolderMeta)
			} else if data, err := repo.ReadBlob(ctx, it.ID); err == nil {
				yaml.Unmarshal(data, &f.Meta)
				s.cache("f:"+it.ID, f.Meta)
			}
			f.Files = append(f.Files, FileInfo{Path: it.Path, Type: "file", Blob: it.ID, Size: it.Size})
		case IsHTML(it.Path):
			m := s.ArtifactMetaOf(ctx, project, it.ID, it.Name)
			f.Artifacts = append(f.Artifacts, Artifact{Path: it.Path, Name: it.Name, Blob: it.ID, Title: m.Title, Width: m.Width, Height: m.Height})
		default:
			f.Files = append(f.Files, FileInfo{Path: it.Path, Type: "file", Blob: it.ID, Size: it.Size})
		}
	}
	order := f.Meta.Order
	if dir == "" {
		if m := s.projectMeta(ctx, repo, commit); len(m.Order) > 0 {
			order = m.Order
		}
	}
	rank := orderRank(order)
	sortByRank(f.Artifacts, rank, func(a Artifact) string { return a.Name })
	sortByRank(f.Folders, rank, func(fi FileInfo) string { return path.Base(fi.Path) })
	return f, nil
}

// orderRank ranks the names an order lists: artifacts, with or without
// ".html", and folders, with or without a trailing "/".
func orderRank(order []string) map[string]int {
	rank := map[string]int{}
	for i, o := range order {
		o = strings.TrimSuffix(strings.TrimSpace(o), "/")
		if _, ok := rank[o]; !ok {
			rank[o] = i + 1
		}
		if !IsHTML(o) {
			if _, ok := rank[o+".html"]; !ok {
				rank[o+".html"] = i + 1
			}
		}
	}
	return rank
}

// sortByRank puts the listed entries first, in the order's order, then the
// rest by name.
func sortByRank[T any](items []T, rank map[string]int, name func(T) string) {
	sort.SliceStable(items, func(i, j int) bool {
		ni, nj := name(items[i]), name(items[j])
		ri, rj := rank[ni], rank[nj]
		switch {
		case ri > 0 && rj > 0:
			return ri < rj
		case ri > 0:
			return true
		case rj > 0:
			return false
		}
		return ni < nj
	})
}

var (
	titlePattern  = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	metaPattern   = regexp.MustCompile(`(?is)<meta\b[^>]*>`)
	attrPattern   = regexp.MustCompile(`(?is)([a-z-]+)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	widthPattern  = regexp.MustCompile(`(?i)\bwidth\s*=\s*(\d+)`)
	heightPattern = regexp.MustCompile(`(?i)\bheight\s*=\s*(\d+)`)
)

// ArtifactMetaOf reads an artifact's title and size (design decision 12).
func (s *Service) ArtifactMetaOf(ctx context.Context, project, blob, name string) ArtifactMeta {
	if v, ok := s.cached("a:" + blob); ok {
		m := v.(ArtifactMeta)
		if m.Title == "" {
			m.Title = name
		}
		return m
	}
	var m ArtifactMeta
	if data, err := s.Repo(project).ReadBlob(ctx, blob); err == nil {
		m = ParseArtifactMeta(data)
	}
	s.cache("a:"+blob, m)
	if m.Title == "" {
		m.Title = name
	}
	return m
}

// ParseArtifactMeta reads <title> and
// <meta name="artifact" content="width=N, height=N"> from the start of an
// HTML document.
func ParseArtifactMeta(data []byte) ArtifactMeta {
	if len(data) > 256<<10 {
		data = data[:256<<10]
	}
	var m ArtifactMeta
	if t := titlePattern.FindSubmatch(data); t != nil {
		m.Title = strings.Join(strings.Fields(html.UnescapeString(string(t[1]))), " ")
	}
	for _, tag := range metaPattern.FindAll(data, -1) {
		attrs := map[string]string{}
		for _, a := range attrPattern.FindAllSubmatch(tag, -1) {
			v := string(a[3]) + string(a[4]) + string(a[5])
			attrs[strings.ToLower(string(a[1]))] = v
		}
		if strings.EqualFold(attrs["name"], "artifact") {
			if w := widthPattern.FindStringSubmatch(attrs["content"]); w != nil {
				if n, err := strconv.Atoi(w[1]); err == nil && n >= MinWidth && n <= MaxWidth {
					m.Width = n
				}
			}
			if h := heightPattern.FindStringSubmatch(attrs["content"]); h != nil {
				if n, err := strconv.Atoi(h[1]); err == nil && n >= MinHeight && n <= MaxHeight {
					m.Height = n
				}
			}
		}
	}
	return m
}

// The sizes an artifact may give itself, in pixels.
const (
	MinWidth, MaxWidth   = 200, 8000
	MinHeight, MaxHeight = 100, 16000
)

// Parent returns the folder of a path ("" for the root).
func Parent(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
