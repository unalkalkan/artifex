package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/store"
)

// FileChange is one change of a publish (design decision 7).
type FileChange struct {
	Path string `json:"path" jsonschema:"file path inside the project, e.g. settings/billing.html"`
	// Content is the new text content. Use ContentBase64 for binary files.
	Content *string `json:"content,omitempty" jsonschema:"new text content of the file (UTF-8)"`
	// ContentBase64 is the new content, base64-encoded.
	ContentBase64 *string `json:"content_base64,omitempty" jsonschema:"new content, base64-encoded; use for binary files such as fonts and images"`
	// Upload is a file sent to an upload address (design decision 23), by
	// the id the address answered with.
	Upload string `json:"upload,omitempty" jsonschema:"id of a file sent to an upload address (create_upload), instead of content: the file's content never passes through the conversation"`
	// Base is the blob id of the version of this file the change starts from.
	Base string `json:"base,omitempty" jsonschema:"blob id of the file version this change is based on, from read_file, list_files or an earlier publish; required to overwrite, move or delete an existing file; omit for a new file"`
	// MoveTo moves (renames) the file; Content may change it at the same time.
	MoveTo string `json:"move_to,omitempty" jsonschema:"new path, to move or rename the file; threads follow it"`
	Delete bool   `json:"delete,omitempty" jsonschema:"delete the file"`
}

// PublishedFile is one file a publish wrote, moved or deleted.
type PublishedFile struct {
	Path    string `json:"path"`
	Blob    string `json:"blob,omitempty"` // the new base for the next change
	Deleted bool   `json:"deleted,omitempty"`
	URL     string `json:"url,omitempty"`
}

// PublishResult is what a publish returns.
type PublishResult struct {
	Project        string          `json:"project"`
	Version        string          `json:"version"`
	ProjectCreated bool            `json:"project_created,omitempty"`
	Unchanged      bool            `json:"unchanged,omitempty"`
	Files          []PublishedFile `json:"files"`
	Warnings       []string        `json:"warnings,omitempty"`
	URL            string          `json:"url"`
}

// ConflictFile is a file that changed since the base a change named.
type ConflictFile struct {
	Path          string `json:"path"`
	Base          string `json:"base,omitempty"`
	Current       string `json:"current_blob,omitempty"` // "" when the file doesn't exist now
	Reason        string `json:"reason"`
	Content       string `json:"current_content,omitempty"`
	ContentBase64 string `json:"current_content_base64,omitempty"`
}

// ConflictError means nothing was written: some files changed since their base.
type ConflictError struct {
	Version string         `json:"version"`
	Files   []ConflictFile `json:"conflicts"`
}

func (e *ConflictError) Error() string {
	paths := make([]string, len(e.Files))
	for i, f := range e.Files {
		paths[i] = f.Path + " (" + f.Reason + ")"
	}
	return "nothing was published: " + strings.Join(paths, "; ")
}

const (
	maxConflictContent = 256 << 10 // per file
	maxConflictTotal   = 1 << 20   // per answer
)

type plannedChange struct {
	src, dst string // src: the path at head; dst: the path after (equal unless moved)
	data     []byte
	hasData  bool
	upload   string // a blob an upload address stored: the new content
	del      bool
	base     string
}

// writes reports whether the change writes new content.
func (pc plannedChange) writes() bool { return pc.hasData || pc.upload != "" }

// Publish writes, moves and deletes files as one commit. Each change to an
// existing file names its base; if any of those files changed since, nothing
// is written and a *ConflictError carries their current content.
func (s *Service) Publish(ctx context.Context, a Actor, project, message string, changes []FileChange) (PublishResult, error) {
	if _, err := s.Require(ctx, a, project, store.ProjectEditor); err != nil {
		return PublishResult{}, err
	}
	plan, err := s.plan(changes)
	if err != nil {
		return PublishResult{}, err
	}
	if err := s.resolveUploads(ctx, s.Repo(project), plan); err != nil {
		return PublishResult{}, err
	}
	if strings.TrimSpace(message) == "" {
		message = defaultMessage(plan)
	}
	message = withMoves(message, plan)
	unlock := s.Lock(project)
	defer unlock()
	repo := s.Repo(project)
	for attempt := 0; ; attempt++ {
		head, err := repo.Head(ctx)
		if err != nil {
			return PublishResult{}, err
		}
		writes, err := s.check(ctx, repo, head, plan)
		if err != nil {
			return PublishResult{}, err
		}
		now := s.now()
		commit, err := repo.CommitFiles(ctx, head, writes, a.signature(now), s.committer(now), message)
		if errors.Is(err, gitrepo.ErrHeadMoved) && attempt < 2 {
			continue // someone wrote outside the lock; check the bases again
		}
		if errors.Is(err, gitrepo.ErrNoChanges) {
			return s.result(ctx, project, head, plan, true), nil
		}
		if err != nil {
			return PublishResult{}, err
		}
		if err := repo.SyncWorktree(ctx); err != nil {
			s.log.Warn("working tree not synced", zap.String("project", project), zapErr(err))
		}
		res := s.result(ctx, project, commit, plan, false)
		s.hub.Publish(events.Event{Type: "version", Project: project, Version: commit, Paths: changedPaths(plan)})
		s.log.Info("published", zap.String("project", project), zap.String("version", short(commit)), zap.String("by", a.Name()), zap.Int("files", len(plan)))
		return res, nil
	}
}

// UploadArtifact writes one file, creating the project when it doesn't exist
// yet (design decision 6).
func (s *Service) UploadArtifact(ctx context.Context, a Actor, project, p string, content, contentBase64 *string, base, title, message string) (PublishResult, error) {
	return s.PublishOrCreate(ctx, a, project, title, message, []FileChange{{Path: p, Content: content, ContentBase64: contentBase64, Base: base}})
}

// PublishOrCreate is Publish for agents: writing to a project that doesn't
// exist yet creates it, with the actor as its Owner (design decision 6). The
// changes are checked first, so a publish that can't succeed creates nothing.
func (s *Service) PublishOrCreate(ctx context.Context, a Actor, project, title, message string, changes []FileChange) (PublishResult, error) {
	plan, err := s.plan(changes)
	if err != nil {
		return PublishResult{}, err
	}
	for _, pc := range plan {
		if pc.dst == "project.yaml" {
			title = "" // the publish writes project.yaml itself
		}
	}
	created, err := s.EnsureProject(ctx, a, project, title)
	if err != nil {
		return PublishResult{}, err
	}
	res, err := s.Publish(ctx, a, project, message, changes)
	res.ProjectCreated = created
	if err != nil && created {
		var ce *ConflictError
		if !errors.As(err, &ce) {
			return res, fmt.Errorf("project %q was created, but nothing was published: %w", project, err)
		}
	}
	return res, err
}

// EnsureProject creates the project when it doesn't exist yet, and reports
// whether it did. A project id taken by a project the actor can't see is a
// conflict, worded so that the project's existence is all it reveals.
func (s *Service) EnsureProject(ctx context.Context, a Actor, project, title string) (bool, error) {
	if _, err := s.store.ProjectByID(ctx, project); errors.Is(err, store.ErrNotFound) {
		if _, err := s.CreateProject(ctx, a, project, title); err != nil {
			return false, err
		}
		return true, nil
	} else if err != nil {
		return false, err
	}
	if _, err := s.Role(ctx, a, project); KindOf(err) == KindNotFound {
		return false, conflict("project id %q is taken by a project you're not a member of; pick another id, or ask its owner to add you", project)
	} else if err != nil {
		return false, err
	}
	return false, nil
}

func (s *Service) plan(changes []FileChange) ([]plannedChange, error) {
	if len(changes) == 0 {
		return nil, invalid("changes is empty")
	}
	seen := map[string]bool{}
	var total int64
	plan := make([]plannedChange, 0, len(changes))
	for i, c := range changes {
		p, err := CleanPath(c.Path)
		if err != nil {
			return nil, err
		}
		pc := plannedChange{src: p, dst: p, base: strings.TrimSpace(c.Base)}
		if c.MoveTo != "" {
			if pc.dst, err = CleanPath(c.MoveTo); err != nil {
				return nil, err
			}
			if pc.dst == pc.src {
				return nil, invalid("change %d moves %q onto itself", i+1, p)
			}
		}
		kinds := 0
		for _, set := range []bool{c.Content != nil, c.ContentBase64 != nil, c.Upload != ""} {
			if set {
				kinds++
			}
		}
		switch {
		case kinds > 1:
			return nil, invalid("change %d (%s) may have only one of content, content_base64 and upload", i+1, p)
		case c.Upload != "":
			u := strings.ToLower(strings.TrimSpace(c.Upload))
			if (len(u) != 40 && len(u) != 64) || !isHexString(u) {
				return nil, invalid("change %d (%s): upload %q is not an upload id; pass the id an upload address answered with", i+1, p, c.Upload)
			}
			pc.upload = u
		case c.Content != nil:
			pc.data, pc.hasData = []byte(*c.Content), true
		case c.ContentBase64 != nil:
			d, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*c.ContentBase64))
			if err != nil {
				return nil, invalid("change %d (%s): content_base64 is not valid base64", i+1, p)
			}
			pc.data, pc.hasData = d, true
		}
		if c.Delete {
			if pc.writes() || c.MoveTo != "" {
				return nil, invalid("change %d (%s) deletes the file, so it can't also write or move it", i+1, p)
			}
			pc.del = true
		}
		if !pc.writes() && !pc.del && c.MoveTo == "" {
			return nil, invalid("change %d (%s) needs content, content_base64, upload, move_to or delete", i+1, p)
		}
		if (pc.del || c.MoveTo != "") && pc.base == "" {
			return nil, invalid("change %d (%s) needs the base blob id of the file it deletes or moves", i+1, p)
		}
		if pc.base != "" && (len(pc.base) < 7 || !isHexString(pc.base)) {
			return nil, invalid("change %d (%s): base %q is not a blob id; pass the full id from read_file or list_files", i+1, p, pc.base)
		}
		if int64(len(pc.data)) > s.cfg.Limits.MaxFileBytes {
			return nil, invalid("%s is %d bytes; the limit is %d", p, len(pc.data), s.cfg.Limits.MaxFileBytes)
		}
		total += int64(len(pc.data))
		touched := []string{pc.src}
		if pc.dst != pc.src {
			touched = append(touched, pc.dst)
		}
		for _, q := range touched {
			if seen[q] {
				return nil, invalid("%q appears in more than one change", q)
			}
			seen[q] = true
		}
		plan = append(plan, pc)
	}
	if total > s.cfg.Limits.MaxPublishBytes {
		return nil, invalid("the publish is %d bytes; the limit is %d", total, s.cfg.Limits.MaxPublishBytes)
	}
	// A path that is a folder for one change and a file for another can't both hold.
	for _, pc := range plan {
		for q := range seen {
			if strings.HasPrefix(q, pc.dst+"/") && !pc.del {
				return nil, invalid("%q is a file in one change and a folder in another", pc.dst)
			}
		}
	}
	return plan, nil
}

// resolveUploads checks that every upload a change names is a blob in the
// project's repository, within the size limits. An upload id is the blob's
// git id, so any blob of the project would do; that gives nothing away, as
// every blob of a project is readable by its members anyway.
func (s *Service) resolveUploads(ctx context.Context, repo *gitrepo.Repo, plan []plannedChange) error {
	var total int64
	uploads := false
	for _, pc := range plan {
		total += int64(len(pc.data))
		if pc.upload == "" {
			continue
		}
		uploads = true
		typ, size, err := repo.ObjectInfo(ctx, pc.upload)
		if errors.Is(err, gitrepo.ErrNotFound) || (err == nil && typ != "blob") {
			return invalid("%s: upload %s is not in this project; send the file to one of this project's upload addresses (create_upload) first", pc.dst, short(pc.upload))
		}
		if err != nil {
			return err
		}
		if size > s.cfg.Limits.MaxFileBytes {
			return invalid("%s is %d bytes; the limit is %d", pc.dst, size, s.cfg.Limits.MaxFileBytes)
		}
		total += size
	}
	if uploads && total > s.cfg.Limits.MaxPublishBytes {
		return invalid("the publish is %d bytes; the limit is %d", total, s.cfg.Limits.MaxPublishBytes)
	}
	return nil
}

// check compares every change with the branch at head and returns the
// writes, or a *ConflictError.
func (s *Service) check(ctx context.Context, repo *gitrepo.Repo, head string, plan []plannedChange) ([]gitrepo.FileWrite, error) {
	lookup := func(p string) (gitrepo.Entry, bool, error) {
		if head == "" {
			return gitrepo.Entry{}, false, nil
		}
		e, err := repo.Lookup(ctx, head, p)
		if errors.Is(err, gitrepo.ErrNotFound) {
			return e, false, nil
		}
		return e, err == nil, err
	}
	var conflicts []ConflictFile
	var writes []gitrepo.FileWrite
	budget := int64(maxConflictTotal)
	for _, pc := range plan {
		cur, exists, err := lookup(pc.src)
		if err != nil {
			return nil, err
		}
		if exists && cur.Type != "blob" {
			return nil, invalid("%q is a folder", pc.src)
		}
		switch {
		case pc.base == "" && exists:
			conflicts = append(conflicts, s.conflictFile(ctx, repo, pc, cur, "the file already exists; pass its blob id as base to overwrite it", &budget))
			continue
		case pc.base != "" && !exists:
			conflicts = append(conflicts, ConflictFile{Path: pc.src, Base: pc.base, Reason: "the file doesn't exist any more"})
			continue
		case pc.base != "" && !strings.HasPrefix(cur.ID, pc.base):
			conflicts = append(conflicts, s.conflictFile(ctx, repo, pc, cur, "the file changed since your base", &budget))
			continue
		}
		// A file can't be written below a path that is a file at head.
		if !pc.del {
			for d := Parent(pc.dst); d != ""; d = Parent(d) {
				e, ok, err := lookup(d)
				if err != nil {
					return nil, err
				}
				if ok && e.Type == "blob" && !removedIn(plan, d) {
					return nil, invalid("%q can't be written: %q is a file", pc.dst, d)
				}
			}
		}
		if pc.dst != pc.src {
			if dstEntry, dstExists, err := lookup(pc.dst); err != nil {
				return nil, err
			} else if dstExists {
				conflicts = append(conflicts, s.conflictFile(ctx, repo, plannedChange{src: pc.dst, upload: pc.upload}, dstEntry, "the destination of the move already exists", &budget))
				continue
			}
			writes = append(writes, gitrepo.FileWrite{Path: pc.src, Remove: true})
		}
		switch {
		case pc.del:
			writes = append(writes, gitrepo.FileWrite{Path: pc.src, Remove: true})
		case pc.hasData:
			writes = append(writes, gitrepo.FileWrite{Path: pc.dst, Data: pc.data})
		case pc.upload != "":
			writes = append(writes, gitrepo.FileWrite{Path: pc.dst, BlobID: pc.upload})
		default: // a move without new content keeps the blob
			writes = append(writes, gitrepo.FileWrite{Path: pc.dst, BlobID: cur.ID, Mode: cur.Mode})
		}
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Version: head, Files: conflicts}
	}
	return writes, nil
}

// conflictFile describes a file that blocks a publish. Its current content
// comes along, up to a total budget for the whole answer, so the agent can
// merge; not for an upload, whose sender works from files on its disk and
// can read_file what it needs.
func (s *Service) conflictFile(ctx context.Context, repo *gitrepo.Repo, pc plannedChange, cur gitrepo.Entry, reason string, budget *int64) ConflictFile {
	cf := ConflictFile{Path: pc.src, Base: pc.base, Current: cur.ID, Reason: reason}
	if pc.upload == "" && cur.Size <= maxConflictContent && cur.Size <= *budget {
		*budget -= cur.Size
		if data, err := repo.ReadBlob(ctx, cur.ID); err == nil {
			if utf8.Valid(data) {
				cf.Content = string(data)
			} else {
				cf.ContentBase64 = base64.StdEncoding.EncodeToString(data)
			}
		}
	}
	return cf
}

func (s *Service) result(ctx context.Context, project, commit string, plan []plannedChange, unchanged bool) PublishResult {
	res := PublishResult{Project: project, Version: commit, Unchanged: unchanged, URL: s.AppURL(project, "", "")}
	repo := s.Repo(project)
	for _, pc := range plan {
		if pc.del {
			res.Files = append(res.Files, PublishedFile{Path: pc.src, Deleted: true})
			continue
		}
		pf := PublishedFile{Path: pc.dst}
		if e, err := repo.Lookup(ctx, commit, pc.dst); err == nil {
			pf.Blob = e.ID
		}
		if IsHTML(pc.dst) {
			pf.URL = s.AppURL(project, pc.dst, "")
		}
		if pc.dst != pc.src {
			res.Files = append(res.Files, PublishedFile{Path: pc.src, Deleted: true})
		}
		res.Files = append(res.Files, pf)
	}
	if commit != "" {
		res.Warnings = s.warnings(ctx, project, commit, plan)
	}
	return res
}

func (s *Service) committer(now time.Time) gitrepo.Signature {
	host := "artifex"
	if u, err := url.Parse(s.cfg.BaseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return gitrepo.Signature{Name: "Artifex", Email: "artifex@" + host, When: now}
}

func defaultMessage(plan []plannedChange) string {
	if len(plan) == 1 {
		pc := plan[0]
		switch {
		case pc.del:
			return "Delete " + pc.src
		case pc.dst != pc.src:
			return "Move " + pc.src + " to " + pc.dst
		}
		return "Update " + pc.dst
	}
	return fmt.Sprintf("Update %d files", len(plan))
}

// withMoves records each move in the commit message, so threads can follow
// an artifact even when it was rewritten in the same commit (§5.3).
func withMoves(message string, plan []plannedChange) string {
	var lines []string
	for _, pc := range plan {
		if pc.dst != pc.src {
			lines = append(lines, gitrepo.MovedLine(pc.src, pc.dst))
		}
	}
	if len(lines) == 0 {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + strings.Join(lines, "\n") + "\n"
}

func changedPaths(plan []plannedChange) []string {
	var out []string
	for _, pc := range plan {
		out = append(out, pc.dst)
		if pc.dst != pc.src {
			out = append(out, pc.src)
		}
	}
	return out
}

var (
	// linkPattern finds the files an HTML file names in href and src: links
	// to other artifacts as well as the files it loads.
	linkPattern = regexp.MustCompile(`(?i)\b(?:href|src)\s*=\s*["']([^"'#?]+)`)
	// loadTag finds elements that load a file when the page opens.
	loadTag  = regexp.MustCompile(`(?is)<(script|link|img|source|video|audio|track|iframe|embed|object|input|image|use)\b[^>]*>`)
	loadAttr = regexp.MustCompile(`(?is)\s(src|href|data|poster|srcset|xlink:href)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	relAttr  = regexp.MustCompile(`(?is)\srel\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	// CSS references, in stylesheets, <style> elements and style attributes.
	cssURL     = regexp.MustCompile(`(?i)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s]*))\s*\)`)
	cssImport  = regexp.MustCompile(`(?i)@import\s+(?:"([^"]*)"|'([^']*)')`)
	styleBlock = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`)
	styleAttr  = regexp.MustCompile(`(?is)\sstyle\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	// schemePattern is a URL that names its own scheme, such as https: or data:.
	schemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// loadingRel are the <link> kinds that load the file they name.
var loadingRel = map[string]bool{"stylesheet": true, "icon": true, "preload": true, "modulepreload": true, "manifest": true, "apple-touch-icon": true, "mask-icon": true}

func firstGroup(m [][]byte) string {
	for _, g := range m[1:] {
		if len(g) > 0 {
			return string(g)
		}
	}
	return ""
}

// isOutside reports whether a reference names another host: what the
// artifact's content policy blocks (design decision 11).
func isOutside(ref string) bool {
	l := strings.ToLower(ref)
	return strings.HasPrefix(l, "http:") || strings.HasPrefix(l, "https:") || strings.HasPrefix(l, "//") || strings.HasPrefix(l, "ws:") || strings.HasPrefix(l, "wss:")
}

// refCheck collects what one file's references would do when viewed.
type refCheck struct {
	file    string // the file the references are in
	dir     string // the folder relative references start from
	has     func(string) bool
	missing map[string]bool
	outside map[string]bool
	rooted  map[string]bool
}

func newRefCheck(file string, has func(string) bool) *refCheck {
	return &refCheck{file: file, dir: Parent(file), has: has, missing: map[string]bool{}, outside: map[string]bool{}, rooted: map[string]bool{}}
}

// project checks a reference to a file of the project.
func (c *refCheck) project(ref string) {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexAny(ref, "#?"); i >= 0 {
		ref = ref[:i]
	}
	if ref == "" || schemePattern.MatchString(ref) || strings.HasPrefix(ref, "//") || strings.HasPrefix(ref, "{") {
		return
	}
	if strings.HasPrefix(ref, "/") {
		// The browser resolves it against the Artifex server, not the project.
		c.rooted[ref] = true
		return
	}
	target := path.Clean(path.Join(c.dir, ref))
	if u, err := url.PathUnescape(target); err == nil {
		target = u
	}
	if target == "." || strings.HasPrefix(target, "..") {
		c.missing[ref+" (points outside the project)"] = true
		return
	}
	if !c.has(target) {
		c.missing[ref] = true
	}
}

// load checks a file the page loads by itself: it must not be on another host.
func (c *refCheck) load(ref string) {
	ref = strings.TrimSpace(ref)
	if isOutside(ref) {
		c.outside[ref] = true
		return
	}
	c.project(ref)
}

func (c *refCheck) css(data []byte) {
	for _, m := range cssURL.FindAllSubmatch(data, -1) {
		if ref := firstGroup(m); ref != "" && !strings.HasPrefix(ref, "#") {
			c.load(ref)
		}
	}
	for _, m := range cssImport.FindAllSubmatch(data, -1) {
		c.load(firstGroup(m))
	}
}

func (c *refCheck) html(data []byte) {
	for _, m := range linkPattern.FindAllSubmatch(data, -1) {
		c.project(string(m[1]))
	}
	for _, tag := range loadTag.FindAllSubmatch(data, -1) {
		name := strings.ToLower(string(tag[1]))
		if name == "link" {
			rel := ""
			if m := relAttr.FindSubmatch(tag[0]); m != nil {
				rel = strings.ToLower(firstGroup(m))
			}
			loads := false
			for _, r := range strings.Fields(rel) {
				loads = loads || loadingRel[r]
			}
			if !loads {
				continue
			}
		}
		for _, m := range loadAttr.FindAllSubmatch(tag[0], -1) {
			v := html.UnescapeString(firstGroup(m[1:]))
			if strings.EqualFold(string(m[1]), "srcset") {
				for _, part := range strings.Split(v, ",") {
					if f := strings.Fields(part); len(f) > 0 {
						c.load(f[0])
					}
				}
				continue
			}
			c.load(v)
		}
	}
	for _, m := range styleBlock.FindAllSubmatch(data, -1) {
		c.css(m[1])
	}
	for _, m := range styleAttr.FindAllSubmatch(data, -1) {
		c.css([]byte(html.UnescapeString(firstGroup(m))))
	}
}

func (c *refCheck) warnings() []string {
	var out []string
	for _, ref := range sortedKeys(c.outside) {
		out = append(out, fmt.Sprintf("%s: %s is on another host, so the browser will block it: artifacts load files only from their own project. Copy the file into the project and use a relative path", c.file, ref))
	}
	for _, ref := range sortedKeys(c.rooted) {
		out = append(out, fmt.Sprintf("%s: %s starts with /, which points at the Artifex server, not the project: use a relative path", c.file, ref))
	}
	for _, ref := range sortedKeys(c.missing) {
		out = append(out, fmt.Sprintf("%s: %s doesn't exist in the project", c.file, ref))
	}
	return out
}

// maxCheckBytes is the largest file whose references a publish checks.
const maxCheckBytes = 4 << 20

// warnings lists what will go wrong when the files just written are viewed:
// references to files that don't exist, files on other hosts that the
// content policy blocks, and YAML entries naming nothing. They never stop a
// publish (design decision 12).
func (s *Service) warnings(ctx context.Context, project, commit string, plan []plannedChange) []string {
	repo := s.Repo(project)
	var out []string
	exists := map[string]bool{}
	has := func(p string) bool {
		if v, ok := exists[p]; ok {
			return v
		}
		_, err := repo.Lookup(ctx, commit, p)
		exists[p] = err == nil
		return exists[p]
	}
	dataOf := func(pc plannedChange) []byte {
		if pc.hasData {
			return pc.data
		}
		if _, size, err := repo.ObjectInfo(ctx, pc.upload); err != nil || size > maxCheckBytes {
			return nil
		}
		data, _ := repo.ReadBlob(ctx, pc.upload)
		return data
	}
	orderEntries := func(file, dir string, order []string) {
		for _, o := range order {
			name := strings.TrimSuffix(strings.TrimSpace(o), "/")
			p := path.Join(dir, name)
			if !has(p) && !has(p+".html") {
				out = append(out, fmt.Sprintf("%s lists %q, which doesn't exist", file, name))
			}
		}
	}
	for _, pc := range plan {
		if pc.del || !pc.writes() {
			continue
		}
		switch ext := strings.ToLower(path.Ext(pc.dst)); {
		case IsHTML(pc.dst):
			if data := dataOf(pc); data != nil {
				c := newRefCheck(pc.dst, has)
				c.html(data)
				out = append(out, c.warnings()...)
			}
		case ext == ".css":
			if data := dataOf(pc); data != nil {
				c := newRefCheck(pc.dst, has)
				c.css(data)
				out = append(out, c.warnings()...)
			}
		case path.Base(pc.dst) == "folder.yaml":
			if f, err := s.FolderAt(ctx, project, commit, Parent(pc.dst)); err == nil {
				orderEntries(pc.dst, Parent(pc.dst), f.Meta.Order)
			}
		case pc.dst == "project.yaml":
			orderEntries(pc.dst, "", s.projectMeta(ctx, repo, commit).Order)
		}
	}
	return out
}

func removedIn(plan []plannedChange, p string) bool {
	for _, pc := range plan {
		if pc.src == p && (pc.del || pc.dst != pc.src) {
			return true
		}
	}
	return false
}

func isHexString(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func zapErr(err error) zap.Field { return zap.Error(err) }
