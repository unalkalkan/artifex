package githttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitrepo.Env(), "GIT_AUTHOR_NAME=Agent", "GIT_AUTHOR_EMAIL=agent@x.test", "GIT_COMMITTER_NAME=Agent", "GIT_COMMITTER_EMAIL=agent@x.test")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCloneAndPush(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{BaseURL: "http://artifex.test", DataDir: dir}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes = 1<<20, 1<<20
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := events.NewHub()
	svc := service.New(cfg, st, hub, zap.NewNop())
	ctx := context.Background()
	owner, _ := svc.AddAccount(ctx, "o@x.test", "O", false)
	viewer, _ := svc.AddAccount(ctx, "v@x.test", "V", false)
	content := "<title>One</title>"
	if _, err := svc.UploadArtifact(ctx, service.Actor{Account: owner}, "demo", "index.html", &content, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	svc.SetMember(ctx, service.Actor{Account: owner}, "demo", "v@x.test", "commenter")
	ot, _ := svc.CreateToken(ctx, owner, "Claude")
	vt, _ := svc.CreateToken(ctx, viewer, "Claude")

	h, err := New(svc, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/git/", h)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ch, cancel := hub.Subscribe("demo")
	defer cancel()

	work := t.TempDir()
	// A wrong token can't clone.
	if out, err := git(t, work, "clone", "http://x:afx_wrong@"+host+"/git/demo.git", "bad"); err == nil {
		t.Fatalf("clone with a bad token: %s", out)
	}
	if out, err := git(t, work, "clone", "http://x:"+ot.Secret+"@"+host+"/git/demo.git", "c"); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	clone := filepath.Join(work, "c")
	os.WriteFile(filepath.Join(clone, "two.html"), []byte("<title>Two</title>"), 0o644)
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-qm", "Add two")
	if out, err := git(t, clone, "push", "origin", "HEAD:main"); err != nil {
		t.Fatalf("push: %v %s", err, out)
	}
	fc, err := svc.ReadFile(ctx, service.Actor{Account: owner}, "demo", "two.html", "")
	if err != nil || fc.Content != "<title>Two</title>" {
		t.Fatalf("pushed file: %v %+v", err, fc)
	}
	// The working tree followed the push (receive.denyCurrentBranch=updateInstead).
	if b, err := os.ReadFile(filepath.Join(dir, "projects", "demo", "two.html")); err != nil || string(b) != "<title>Two</title>" {
		t.Fatalf("working tree: %v %q", err, b)
	}
	select {
	case ev := <-ch:
		if ev.Type != "version" || len(ev.Paths) != 1 || ev.Paths[0] != "two.html" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after push")
	}
	// History can't be rewritten.
	git(t, clone, "reset", "-q", "--hard", "HEAD~1")
	os.WriteFile(filepath.Join(clone, "three.html"), []byte("x"), 0o644)
	git(t, clone, "add", ".")
	git(t, clone, "commit", "-qm", "rewrite")
	if out, err := git(t, clone, "push", "-f", "origin", "HEAD:main"); err == nil {
		t.Fatalf("force push accepted: %s", out)
	}
	// A commenter can clone but not push.
	if out, err := git(t, work, "clone", "http://x:"+vt.Secret+"@"+host+"/git/demo.git", "v"); err != nil {
		t.Fatalf("commenter clone: %v %s", err, out)
	}
	vclone := filepath.Join(work, "v")
	os.WriteFile(filepath.Join(vclone, "four.html"), []byte("x"), 0o644)
	git(t, vclone, "add", ".")
	git(t, vclone, "commit", "-qm", "four")
	if out, err := git(t, vclone, "push", "origin", "HEAD:main"); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("commenter push: %v %s", err, out)
	}
}
