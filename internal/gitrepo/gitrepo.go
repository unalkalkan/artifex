// Package gitrepo runs the git program against one project repository.
//
// Every read names a commit, so what it returns never changes. Writes build a
// commit with plumbing commands in a private index and move the branch with
// `git update-ref <branch> <new> <old>`: the branch moves only if nobody moved
// it since, which is git's own guarantee that no commit is lost (design
// decision 8).
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Branch is the only branch Artifex shows.
const Branch = "refs/heads/main"

var (
	// ErrNotFound means the path does not exist at that commit.
	ErrNotFound = errors.New("not found")
	// ErrHeadMoved means the branch moved after the caller read it.
	ErrHeadMoved = errors.New("branch moved")
	// ErrNoChanges means a commit would have the same tree as its parent.
	ErrNoChanges = errors.New("no changes")
	// ErrBadRevision means a version string is not a commit of this repository.
	ErrBadRevision = errors.New("unknown version")
)

// Repo is one project repository: a normal folder with a .git directory.
// The working tree is kept equal to the branch, as a convenience for anyone
// looking at the host; every read comes from git objects, never from it.
type Repo struct {
	Dir string
}

// Open returns the repository at dir without checking it.
func Open(dir string) *Repo { return &Repo{Dir: dir} }

// Init creates a repository at dir, which must not exist yet.
func Init(ctx context.Context, dir string) (*Repo, error) {
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	r := &Repo{Dir: dir}
	// An empty template: no sample hooks, nothing copied from the host.
	if _, err := r.run(ctx, nil, "init", "-q", "--template=", "-b", "main"); err != nil {
		return nil, err
	}
	settings := [][2]string{
		// A push updates the checked-out working tree (git over HTTPS, phase 3).
		{"receive.denyCurrentBranch", "updateInstead"},
		// History can't be rewritten or deleted by a push: every version stays.
		{"receive.denyNonFastForwards", "true"},
		{"receive.denyDeletes", "true"},
		{"core.quotePath", "false"},
		{"gc.auto", "0"},
	}
	for _, s := range settings {
		if _, err := r.run(ctx, nil, "config", s[0], s[1]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Signature is a commit author.
type Signature struct {
	Name  string
	Email string
	When  time.Time
}

// Entry is one item of a tree.
type Entry struct {
	Path string // full path from the repository root
	Name string // last segment
	Type string // "blob" or "tree"
	Mode string
	ID   string
	Size int64 // blobs only
}

// Commit is one commit of a log.
type Commit struct {
	ID          string
	Parents     []string
	AuthorName  string
	AuthorEmail string
	When        time.Time
	Subject     string
	Body        string
}

// Head returns the branch's commit, or "" when the repository has none.
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "-q", Branch+"^{commit}")
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) && ee.code == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveCommit turns a version string into a full commit id. An empty
// string means the branch's head ("" when the repository has no commit).
func (r *Repo) ResolveCommit(ctx context.Context, rev string) (string, error) {
	if rev == "" || rev == "HEAD" || rev == "main" {
		return r.Head(ctx)
	}
	if !isHex(rev) || len(rev) < 4 || len(rev) > 64 {
		return "", ErrBadRevision
	}
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "-q", rev+"^{commit}")
	if err != nil {
		return "", ErrBadRevision
	}
	return strings.TrimSpace(string(out)), nil
}

// Lookup returns the entry at path in commit. The empty path is the root tree.
func (r *Repo) Lookup(ctx context.Context, commit, path string) (Entry, error) {
	if commit == "" {
		return Entry{}, ErrNotFound
	}
	if path == "" {
		out, err := r.run(ctx, nil, "rev-parse", commit+"^{tree}")
		if err != nil {
			return Entry{}, err
		}
		return Entry{Type: "tree", Mode: "040000", ID: strings.TrimSpace(string(out))}, nil
	}
	out, err := r.run(ctx, nil, "ls-tree", "-z", "-l", "--full-tree", commit, "--", path)
	if err != nil {
		return Entry{}, err
	}
	for _, item := range parseLsTree(out) {
		if item.Path == path {
			return item, nil
		}
	}
	return Entry{}, ErrNotFound
}

// ReadBlob returns the content of a blob by id.
func (r *Repo) ReadBlob(ctx context.Context, id string) ([]byte, error) {
	if !isHex(id) {
		return nil, ErrNotFound
	}
	return r.run(ctx, nil, "cat-file", "blob", id)
}

// StreamBlob writes a blob's content to w without holding it in memory.
func (r *Repo) StreamBlob(ctx context.Context, id string, w io.Writer) error {
	if !isHex(id) {
		return ErrNotFound
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", id)
	cmd.Dir = r.Dir
	cmd.Env = Env()
	cmd.Stdout = w
	return cmd.Run()
}

// WriteBlob stores src's content as a blob, streamed, and returns its id.
// Nothing refers to the blob until a commit does; git removes unreferenced
// objects only once they are two weeks old.
func (r *Repo) WriteBlob(ctx context.Context, src io.Reader) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	cmd.Dir = r.Dir
	cmd.Env = Env()
	cmd.Stdin = src
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", &exitError{code: ee.ExitCode(), stderr: stderr.String(), args: []string{"hash-object", "-w", "--stdin"}}
		}
		return "", err // src's own error too, such as a size limit
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ObjectInfo returns an object's type ("blob", "tree", "commit") and size,
// or ErrNotFound when the repository doesn't have it.
func (r *Repo) ObjectInfo(ctx context.Context, id string) (string, int64, error) {
	if !isHex(id) {
		return "", 0, ErrNotFound
	}
	out, err := r.runInput(ctx, nil, []byte(id+"\n"), "cat-file", "--batch-check")
	if err != nil {
		return "", 0, err
	}
	f := strings.Fields(string(out))
	if len(f) != 3 || f[0] != id {
		return "", 0, ErrNotFound // "<id> missing"
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return "", 0, err
	}
	return f[1], size, nil
}

// ReadFile returns the blob at path in commit.
func (r *Repo) ReadFile(ctx context.Context, commit, path string) (Entry, []byte, error) {
	e, err := r.Lookup(ctx, commit, path)
	if err != nil {
		return Entry{}, nil, err
	}
	if e.Type != "blob" {
		return e, nil, ErrNotFound
	}
	data, err := r.ReadBlob(ctx, e.ID)
	return e, data, err
}

// ListTree lists the tree at dir in commit; recursive lists every blob below it.
func (r *Repo) ListTree(ctx context.Context, commit, dir string, recursive bool) ([]Entry, error) {
	if commit == "" {
		return nil, nil
	}
	treeish := commit
	if dir != "" {
		e, err := r.Lookup(ctx, commit, dir)
		if err != nil {
			return nil, err
		}
		if e.Type != "tree" {
			return nil, ErrNotFound
		}
		treeish = e.ID
	}
	args := []string{"ls-tree", "-z", "-l"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, treeish)
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	items := parseLsTree(out)
	for i := range items {
		if dir != "" {
			items[i].Path = dir + "/" + items[i].Path
		}
	}
	return items, nil
}

// Log lists commits of the branch, newest first. With a path it lists the
// commits that changed that file, following renames.
func (r *Repo) Log(ctx context.Context, from, path string, limit, skip int) ([]Commit, error) {
	if from == "" {
		return nil, nil
	}
	args := []string{"log", "-z", "--format=%H%x1f%P%x1f%an%x1f%ae%x1f%at%x1f%s%x1f%b"}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	if skip > 0 {
		args = append(args, "--skip", strconv.Itoa(skip))
	}
	if path != "" {
		args = append(args, "--follow")
	}
	args = append(args, from)
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range bytes.Split(out, []byte{0}) {
		rec = bytes.TrimLeft(rec, "\n")
		if len(rec) == 0 {
			continue
		}
		f := strings.SplitN(string(rec), "\x1f", 7)
		if len(f) < 7 {
			continue
		}
		at, _ := strconv.ParseInt(f[4], 10, 64)
		c := Commit{ID: f[0], AuthorName: f[2], AuthorEmail: f[3], When: time.Unix(at, 0).UTC(), Subject: f[5], Body: strings.TrimSpace(f[6])}
		if f[1] != "" {
			c.Parents = strings.Fields(f[1])
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// CommitInfo returns one commit.
func (r *Repo) CommitInfo(ctx context.Context, id string) (Commit, error) {
	cs, err := r.Log(ctx, id, "", 1, 0)
	if err != nil {
		return Commit{}, err
	}
	if len(cs) == 0 {
		return Commit{}, ErrBadRevision
	}
	return cs[0], nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) bool {
	_, err := r.run(ctx, nil, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Diff returns the unified source diff between two commits, with renames
// detected. An empty from means the empty tree.
func (r *Repo) Diff(ctx context.Context, from, to, path string) (string, error) {
	if from == "" {
		from = emptyTree
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "-M", from, to}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := r.run(ctx, nil, args...)
	return string(out), err
}

// Change is one line of a name-status diff.
type Change struct {
	Status  byte   // 'A', 'M', 'D', 'R', 'C', 'T'
	Path    string // the new path (the only path for A, M, D, T)
	OldPath string // for R and C
}

// Changes lists what changed between two commits, with renames detected.
func (r *Repo) Changes(ctx context.Context, from, to string) ([]Change, error) {
	if from == "" {
		from = emptyTree
	}
	out, err := r.run(ctx, nil, "diff", "--no-color", "--no-ext-diff", "-M", "--name-status", "-z", from, to)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(out, []byte{0})
	var changes []Change
	for i := 0; i < len(parts); i++ {
		st := string(parts[i])
		if st == "" {
			continue
		}
		c := Change{Status: st[0]}
		switch c.Status {
		case 'R', 'C':
			if i+2 >= len(parts) {
				return changes, nil
			}
			c.OldPath, c.Path = string(parts[i+1]), string(parts[i+2])
			i += 2
		default:
			if i+1 >= len(parts) {
				return changes, nil
			}
			c.Path = string(parts[i+1])
			i++
		}
		changes = append(changes, c)
	}
	return changes, nil
}

// FollowCache remembers what each commit did to paths. A commit never
// changes, so its entry never goes stale; the cache only bounds its size.
type FollowCache struct {
	mu    sync.Mutex
	steps map[string]step     // commit -> its moves and deletions
	pairs map[string][]Change // from+to -> the direct diff
	revs  map[string][]string // from+to -> commits in between, oldest first
}

type step struct {
	moves   map[string]string
	deleted map[string]bool
}

func NewFollowCache() *FollowCache {
	return &FollowCache{steps: map[string]step{}, pairs: map[string][]Change{}, revs: map[string][]string{}}
}

func (c *FollowCache) bound() {
	if len(c.steps) > 200000 {
		c.steps = map[string]step{}
	}
	if len(c.pairs) > 20000 {
		c.pairs = map[string][]Change{}
	}
	if len(c.revs) > 20000 {
		c.revs = map[string][]string{}
	}
}

// FollowPath returns where the file at path in commit from is in commit to,
// following moves; "" when it was deleted. It tries one diff first and walks
// commit by commit only when that diff can't tell: git pairs a rename with
// its edit only when the content stayed similar, while a move a publish
// recorded in its commit message (MovedTrailer) is exact. cache may be nil.
func (r *Repo) FollowPath(ctx context.Context, from, to, path string, cache *FollowCache) (string, error) {
	if from == to || from == "" || to == "" {
		return path, nil
	}
	if cache == nil {
		cache = NewFollowCache()
	}
	changes, err := r.cachedChanges(ctx, from, to, cache)
	if err != nil {
		return "", err
	}
	deleted := false
	for _, c := range changes {
		if c.Status == 'R' && c.OldPath == path {
			return c.Path, nil
		}
		if c.Path == path {
			if c.Status != 'D' {
				return path, nil
			}
			deleted = true
		}
	}
	if !deleted {
		return path, nil
	}
	revs, err := r.cachedRevs(ctx, from, to, cache)
	if err != nil {
		return "", err
	}
	cur, prev := path, from
	for _, c := range revs {
		st, err := r.cachedStep(ctx, prev, c, cache)
		if err != nil {
			return "", err
		}
		if dst, ok := st.moves[cur]; ok {
			cur = dst
		} else if st.deleted[cur] {
			return "", nil
		}
		prev = c
	}
	return cur, nil
}

func (r *Repo) cachedChanges(ctx context.Context, from, to string, c *FollowCache) ([]Change, error) {
	key := from + to
	c.mu.Lock()
	ch, ok := c.pairs[key]
	c.mu.Unlock()
	if ok {
		return ch, nil
	}
	ch, err := r.Changes(ctx, from, to)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.bound()
	c.pairs[key] = ch
	c.mu.Unlock()
	return ch, nil
}

func (r *Repo) cachedRevs(ctx context.Context, from, to string, c *FollowCache) ([]string, error) {
	key := from + to
	c.mu.Lock()
	revs, ok := c.revs[key]
	c.mu.Unlock()
	if ok {
		return revs, nil
	}
	out, err := r.run(ctx, nil, "rev-list", "--reverse", "--topo-order", "--first-parent", from+".."+to)
	if err != nil {
		return nil, err
	}
	revs = strings.Fields(string(out))
	c.mu.Lock()
	c.bound()
	c.revs[key] = revs
	c.mu.Unlock()
	return revs, nil
}

// cachedStep is what commit did relative to its first parent prev: the moves
// a publish recorded, plus the renames and deletions git detects.
func (r *Repo) cachedStep(ctx context.Context, prev, commit string, c *FollowCache) (step, error) {
	c.mu.Lock()
	st, ok := c.steps[commit]
	c.mu.Unlock()
	if ok {
		return st, nil
	}
	st = step{moves: map[string]string{}, deleted: map[string]bool{}}
	recorded, err := r.recordedMoves(ctx, commit)
	if err != nil {
		return st, err
	}
	changes, err := r.Changes(ctx, prev, commit)
	if err != nil {
		return st, err
	}
	for _, ch := range changes {
		switch ch.Status {
		case 'R':
			st.moves[ch.OldPath] = ch.Path
		case 'D':
			st.deleted[ch.Path] = true
		}
	}
	for from, to := range recorded {
		st.moves[from] = to
	}
	c.mu.Lock()
	c.bound()
	c.steps[commit] = st
	c.mu.Unlock()
	return st, nil
}

// MovedTrailer is the commit message line that records a move made by a
// publish: `Artifex-Moved: "old/path" "new/path"`.
const MovedTrailer = "Artifex-Moved:"

// MovedLine formats one MovedTrailer line.
func MovedLine(from, to string) string {
	return MovedTrailer + " " + strconv.Quote(from) + " " + strconv.Quote(to)
}

func (r *Repo) recordedMoves(ctx context.Context, commit string) (map[string]string, error) {
	out, err := r.run(ctx, nil, "log", "-1", "--format=%B", commit)
	if err != nil {
		return nil, err
	}
	moves := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), MovedTrailer)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		from, err := strconv.QuotedPrefix(rest)
		if err != nil {
			continue
		}
		to := strings.TrimSpace(rest[len(from):])
		f, err1 := strconv.Unquote(from)
		t, err2 := strconv.Unquote(to)
		if err1 == nil && err2 == nil {
			moves[f] = t
		}
	}
	return moves, nil
}

// FileWrite is one file of a commit: new content, or a removal.
type FileWrite struct {
	Path   string
	Data   []byte // used when BlobID is empty and Remove is false
	BlobID string // an existing blob, for moves and restores
	Mode   string // defaults to 100644
	Remove bool
}

// CommitFiles builds a commit on parent (the caller's view of the branch, ""
// for the first commit) and moves the branch to it only if the branch is
// still at parent. It returns ErrHeadMoved if it wasn't, and ErrNoChanges if
// the tree would not change.
func (r *Repo) CommitFiles(ctx context.Context, parent string, files []FileWrite, author Signature, committer Signature, message string) (string, error) {
	tmp, err := os.CreateTemp(filepath.Join(r.Dir, ".git"), "artifex-index-")
	if err != nil {
		return "", err
	}
	idx := tmp.Name()
	tmp.Close()
	os.Remove(idx) // git creates it; an empty file is not a valid index
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}

	if parent != "" {
		if _, err := r.run(ctx, env, "read-tree", parent); err != nil {
			return "", err
		}
	}
	var info bytes.Buffer
	for _, f := range files {
		if f.Remove {
			fmt.Fprintf(&info, "0 %s\t%s\x00", zeroID, f.Path)
			continue
		}
		id := f.BlobID
		if id == "" {
			out, err := r.runInput(ctx, nil, f.Data, "hash-object", "-w", "--stdin")
			if err != nil {
				return "", err
			}
			id = strings.TrimSpace(string(out))
		}
		mode := f.Mode
		if mode == "" {
			mode = "100644"
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", mode, id, f.Path)
	}
	if info.Len() > 0 {
		// -z: NUL-separated, so paths are taken as they are, never C-unquoted.
		if _, err := r.runInput(ctx, env, info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
			return "", err
		}
	}
	treeOut, err := r.run(ctx, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(treeOut))
	if parent != "" {
		pt, err := r.run(ctx, nil, "rev-parse", parent+"^{tree}")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(pt)) == tree {
			return "", ErrNoChanges
		}
	} else if tree == emptyTree {
		return "", ErrNoChanges
	}

	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	cenv := []string{
		"GIT_AUTHOR_NAME=" + clean(author.Name), "GIT_AUTHOR_EMAIL=" + clean(author.Email), "GIT_AUTHOR_DATE=" + gitDate(author.When),
		"GIT_COMMITTER_NAME=" + clean(committer.Name), "GIT_COMMITTER_EMAIL=" + clean(committer.Email), "GIT_COMMITTER_DATE=" + gitDate(committer.When),
	}
	out, err := r.runInput(ctx, cenv, []byte(message), args...)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(out))

	old := parent
	if old == "" {
		old = zeroID
	}
	if _, err := r.run(ctx, nil, "update-ref", "-m", "artifex", Branch, commit, old); err != nil {
		return "", ErrHeadMoved
	}
	return commit, nil
}

// SyncWorktree makes the working tree equal to the branch.
func (r *Repo) SyncWorktree(ctx context.Context) error {
	head, err := r.Head(ctx)
	if err != nil || head == "" {
		return err
	}
	_, err = r.run(ctx, nil, "reset", "-q", "--hard", head)
	return err
}

const (
	zeroID    = "0000000000000000000000000000000000000000"
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

func parseLsTree(out []byte) []Entry {
	var items []Entry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		tab := bytes.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(string(rec[:tab]))
		if len(meta) < 4 {
			continue
		}
		e := Entry{Mode: meta[0], Type: meta[1], ID: meta[2], Path: string(rec[tab+1:])}
		if meta[3] != "-" {
			e.Size, _ = strconv.ParseInt(meta[3], 10, 64)
		}
		e.Name = e.Path[strings.LastIndexByte(e.Path, '/')+1:]
		if e.Type == "blob" || e.Type == "tree" {
			items = append(items, e)
		}
	}
	return items
}

func gitDate(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return fmt.Sprintf("%d +0000", t.Unix())
}

// clean keeps names and emails from breaking a commit header.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' || r == '\r' || r == 0 {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type exitError struct {
	code   int
	stderr string
	args   []string
}

func (e *exitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.args, " "), e.code, strings.TrimSpace(e.stderr))
}

func (r *Repo) run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return r.runInput(ctx, env, nil, args...)
}

func (r *Repo) runInput(ctx context.Context, env []string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(Env(), env...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &exitError{code: ee.ExitCode(), stderr: stderr.String(), args: args}
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// Env is the environment every git process runs with: no system or user
// configuration, no prompts, a fixed locale.
func Env() []string {
	keep := []string{}
	for _, k := range []string{"PATH", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			keep = append(keep, k+"="+v)
		}
	}
	return append(keep,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		// Paths are file names, never pathspec magic (":(glob)…", ":!…").
		"GIT_LITERAL_PATHSPECS=1",
		"HOME=/nonexistent",
		"LC_ALL=C",
	)
}
