package gitrepo

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var who = Signature{Name: "Tester", Email: "t@example.com", When: time.Unix(1700000000, 0)}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Init(context.Background(), filepath.Join(t.TempDir(), "p"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCommitReadAndHistory(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	if h, _ := r.Head(ctx); h != "" {
		t.Fatalf("new repo has head %q", h)
	}
	c1, err := r.CommitFiles(ctx, "", []FileWrite{{Path: "a/index.html", Data: []byte("<p>one</p>")}, {Path: "style.css", Data: []byte("p{}")}}, who, who, "first")
	if err != nil {
		t.Fatal(err)
	}
	e, data, err := r.ReadFile(ctx, c1, "a/index.html")
	if err != nil || string(data) != "<p>one</p>" || e.Size != 10 {
		t.Fatalf("read: %v %q %+v", err, data, e)
	}
	if _, _, err := r.ReadFile(ctx, c1, "missing.html"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, _, err := r.ReadFile(ctx, c1, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tree read as file: %v", err)
	}

	// Rename and edit in one commit; history of the new path follows it.
	c2, err := r.CommitFiles(ctx, c1, []FileWrite{{Path: "a/index.html", Remove: true}, {Path: "b/home.html", BlobID: e.ID}}, who, who, "move")
	if err != nil {
		t.Fatal(err)
	}
	c3, err := r.CommitFiles(ctx, c2, []FileWrite{{Path: "b/home.html", Data: []byte("<p>one</p>\n<p>two</p>")}}, who, who, "edit")
	if err != nil {
		t.Fatal(err)
	}
	log, err := r.Log(ctx, c3, "b/home.html", 0, 0)
	if err != nil || len(log) != 3 {
		t.Fatalf("follow log: %v %d", err, len(log))
	}
	if log[0].Subject != "edit" || log[2].Subject != "first" {
		t.Fatalf("log order: %+v", log)
	}
	// One diff from c1 to c3 sees a delete and an add (the file changed too
	// much); following commit by commit still finds the move.
	if p, err := r.FollowPath(ctx, c1, c3, "a/index.html", nil); err != nil || p != "b/home.html" {
		t.Fatalf("follow: %v %q", err, p)
	}
	if p, err := r.FollowPath(ctx, c1, c3, "style.css", nil); err != nil || p != "style.css" {
		t.Fatalf("unchanged: %v %q", err, p)
	}
	c4, err := r.CommitFiles(ctx, c3, []FileWrite{{Path: "b/home.html", Remove: true}}, who, who, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := r.FollowPath(ctx, c1, c4, "a/index.html", nil); err != nil || p != "" {
		t.Fatalf("deleted: %v %q", err, p)
	}
	if err := r.SyncWorktree(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListTree(ctx, c3, "", true)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %+v", err, list)
	}
	diff, err := r.Diff(ctx, c2, c3, "")
	if err != nil || !strings.Contains(diff, "+<p>two</p>") {
		t.Fatalf("diff: %v %q", err, diff)
	}
}

func TestCommitRefusesMovedBranchAndEmptyChange(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c1, err := r.CommitFiles(ctx, "", []FileWrite{{Path: "x.txt", Data: []byte("1")}}, who, who, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitFiles(ctx, c1, []FileWrite{{Path: "x.txt", Data: []byte("1")}}, who, who, "same"); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("same tree: %v", err)
	}
	if _, err := r.CommitFiles(ctx, c1, []FileWrite{{Path: "x.txt", Data: []byte("2")}}, who, who, "2"); err != nil {
		t.Fatal(err)
	}
	// A writer still holding c1 must not move the branch.
	if _, err := r.CommitFiles(ctx, c1, []FileWrite{{Path: "y.txt", Data: []byte("y")}}, who, who, "stale"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("stale parent: %v", err)
	}
}

func TestConcurrentCommitsLoseNothing(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	base, err := r.CommitFiles(ctx, "", []FileWrite{{Path: "seed", Data: []byte("s")}}, who, who, "seed")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wins := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := r.CommitFiles(ctx, base, []FileWrite{{Path: "f" + string(rune('a'+i)), Data: []byte("x")}}, who, who, "w")
			if err == nil {
				wins <- c
			} else if !errors.Is(err, ErrHeadMoved) {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	n := 0
	var winner string
	for c := range wins {
		n++
		winner = c
	}
	if n != 1 {
		t.Fatalf("%d writers moved the branch from the same parent", n)
	}
	if h, _ := r.Head(ctx); h != winner {
		t.Fatalf("head %s, winner %s", h, winner)
	}
}

func TestResolveCommit(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	c1, _ := r.CommitFiles(ctx, "", []FileWrite{{Path: "x", Data: []byte("1")}}, who, who, "1")
	got, err := r.ResolveCommit(ctx, c1[:10])
	if err != nil || got != c1 {
		t.Fatalf("short id: %v %s", err, got)
	}
	for _, bad := range []string{"--all", "zzzz", "HEAD~1", "abc"} {
		if _, err := r.ResolveCommit(ctx, bad); err == nil {
			t.Fatalf("%q resolved", bad)
		}
	}
}

func TestPathsAreLiteral(t *testing.T) {
	ctx := context.Background()
	r := newRepo(t)
	odd := []string{`"q.html"`, ":notes.html", ":(glob)*.html", "a b/c\td.html"}
	var files []FileWrite
	for _, p := range odd {
		files = append(files, FileWrite{Path: p, Data: []byte(p)})
	}
	files = append(files, FileWrite{Path: "q.html", Data: []byte("plain")}, FileWrite{Path: "notes.html", Data: []byte("plain")})
	c, err := r.CommitFiles(ctx, "", files, who, who, "odd names")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range odd {
		_, data, err := r.ReadFile(ctx, c, p)
		if err != nil || string(data) != p {
			t.Fatalf("%q: %v %q", p, err, data)
		}
	}
	for _, p := range []string{"q.html", "notes.html"} {
		if _, data, err := r.ReadFile(ctx, c, p); err != nil || string(data) != "plain" {
			t.Fatalf("%q was overwritten: %v %q", p, err, data)
		}
	}
	if log, err := r.Log(ctx, c, ":notes.html", 0, 0); err != nil || len(log) != 1 {
		t.Fatalf("log of a ':' path: %v %d", err, len(log))
	}
}
