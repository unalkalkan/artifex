package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/store"
)

type fixture struct {
	svc   *Service
	owner Actor
	other Actor
	admin Actor
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{BaseURL: "http://artifex.test", DataDir: dir}
	cfg.Limits.MaxFileBytes = 1 << 20
	cfg.Limits.MaxPublishBytes = 2 << 20
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := New(cfg, st, events.NewHub(), zap.NewNop())
	ctx := context.Background()
	mk := func(email string, admin bool) Actor {
		a, err := svc.AddAccount(ctx, email, strings.Split(email, "@")[0], admin)
		if err != nil {
			t.Fatal(err)
		}
		return Actor{Account: a, AgentLabel: "Claude"}
	}
	return &fixture{svc: svc, owner: mk("owner@x.test", false), other: mk("other@x.test", false), admin: mk("admin@x.test", true)}
}

func str(s string) *string { return &s }

func TestUploadCreatesProjectAndChecksBase(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.svc.UploadArtifact(ctx, f.owner, "demo", "index.html", str("<title>Hi</title><p>1</p>"), nil, "", "Demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProjectCreated || res.Version == "" || len(res.Files) != 1 || res.Files[0].Blob == "" {
		t.Fatalf("first upload: %+v", res)
	}
	if role, _ := f.svc.Role(ctx, f.owner, "demo"); role != store.ProjectOwner {
		t.Fatalf("creator role %q", role)
	}
	// Overwriting without a base is a conflict that carries the current content.
	_, err = f.svc.UploadArtifact(ctx, f.owner, "demo", "index.html", str("<p>2</p>"), nil, "", "", "")
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Files) != 1 || !strings.Contains(ce.Files[0].Content, "<p>1</p>") {
		t.Fatalf("no-base overwrite: %v", err)
	}
	// With the base from the first result it works, and returns the next base.
	res2, err := f.svc.UploadArtifact(ctx, f.owner, "demo", "index.html", str("<p>2</p>"), nil, res.Files[0].Blob, "", "edit")
	if err != nil || res2.ProjectCreated {
		t.Fatalf("based overwrite: %v %+v", err, res2)
	}
	// The old base is now stale.
	if _, err := f.svc.UploadArtifact(ctx, f.owner, "demo", "index.html", str("<p>3</p>"), nil, res.Files[0].Blob, "", ""); !errors.As(err, &ce) {
		t.Fatalf("stale base: %v", err)
	}
	// Same content again changes nothing.
	res3, err := f.svc.UploadArtifact(ctx, f.owner, "demo", "index.html", str("<p>2</p>"), nil, res2.Files[0].Blob, "", "")
	if err != nil || !res3.Unchanged || res3.Version != res2.Version {
		t.Fatalf("unchanged: %v %+v", err, res3)
	}
	// project.yaml from the title.
	pv, err := f.svc.Project(ctx, f.owner, "demo")
	if err != nil || pv.Title != "Demo" {
		t.Fatalf("project view: %v %+v", err, pv)
	}
}

func TestPublishMovesDeletesAndValidates(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.svc.CreateProject(ctx, f.owner, "p1", ""); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Publish(ctx, f.owner, "p1", "", []FileChange{
		{Path: "a/one.html", Content: str(`<a href="two.html">2</a><img src="../img/x.png"><a href="missing.html">m</a>`)},
		{Path: "a/two.html", Content: str("<p>two</p>")},
		{Path: "img/x.png", ContentBase64: str("iVBORw0KGgo=")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "missing.html") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	blob := map[string]string{}
	for _, pf := range res.Files {
		blob[pf.Path] = pf.Blob
	}
	res, err = f.svc.Publish(ctx, f.owner, "p1", "reorganise", []FileChange{
		{Path: "a/two.html", MoveTo: "b/two.html", Base: blob["a/two.html"]},
		{Path: "img/x.png", Delete: true, Base: blob["img/x.png"]},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := f.svc.ListFiles(ctx, f.owner, "p1", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, fi := range files {
		paths = append(paths, fi.Path)
	}
	if strings.Join(paths, ",") != "a/one.html,b/two.html" {
		t.Fatalf("files after move: %v", paths)
	}
	bad := [][]FileChange{
		{{Path: "../x.html", Content: str("x")}},
		{{Path: ".git/config", Content: str("x")}},
		{{Path: "-/x.html", Content: str("x")}},
		{{Path: "a/one.html", Delete: true}},                                       // no base
		{{Path: "x.html", Content: str("x"), ContentBase64: str("eA==")}},          // both
		{{Path: "y.html", Content: str("1")}, {Path: "y.html", Content: str("2")}}, // twice
		{{Path: "a/one.html/z.html", Content: str("x")}},                           // under a file
		{{Path: "a/one.html", Content: str("x"), Base: "zz"}},                      // not a blob id
	}
	for i, ch := range bad {
		if _, err := f.svc.Publish(ctx, f.owner, "p1", "", ch); KindOf(err) != KindInvalid {
			t.Errorf("bad change %d: got %v", i, err)
		}
	}
}

func TestRolesAndMembers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.svc.UploadArtifact(ctx, f.owner, "team", "index.html", str("x"), nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// Not a member: the project doesn't exist for them.
	if _, err := f.svc.ReadFile(ctx, f.other, "team", "index.html", ""); KindOf(err) != KindNotFound {
		t.Fatalf("outsider read: %v", err)
	}
	if _, err := f.svc.SetMember(ctx, f.owner, "team", "other@x.test", "commenter"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ReadFile(ctx, f.other, "team", "index.html", ""); err != nil {
		t.Fatalf("commenter read: %v", err)
	}
	if _, err := f.svc.Publish(ctx, f.other, "team", "", []FileChange{{Path: "b.html", Content: str("b")}}); KindOf(err) != KindForbidden {
		t.Fatalf("commenter publish: %v", err)
	}
	if _, err := f.svc.SetMember(ctx, f.other, "team", "x@x.test", "editor"); KindOf(err) != KindForbidden {
		t.Fatalf("commenter managing members: %v", err)
	}
	// The only owner can't step down or leave.
	if _, err := f.svc.SetMember(ctx, f.owner, "team", "owner@x.test", "editor"); KindOf(err) != KindConflict {
		t.Fatalf("last owner demoted: %v", err)
	}
	if err := f.svc.RemoveMember(ctx, f.owner, "team", "owner@x.test"); KindOf(err) != KindConflict {
		t.Fatalf("last owner left: %v", err)
	}
	// An Admin acts as an owner without being a member.
	if _, err := f.svc.Publish(ctx, f.admin, "team", "", []FileChange{{Path: "c.html", Content: str("c")}}); err != nil {
		t.Fatalf("admin publish: %v", err)
	}
	// An invitation becomes a membership when the account signs in.
	mv, err := f.svc.SetMember(ctx, f.owner, "team", "new@x.test", "editor")
	if err != nil || !mv.Invited {
		t.Fatalf("invite: %v %+v", err, mv)
	}
	acct, err := f.svc.SignIn(ctx, "new@x.test", "New", true)
	if err != nil {
		t.Fatal(err)
	}
	if role, err := f.svc.Role(ctx, Actor{Account: acct}, "team"); err != nil || role != store.ProjectEditor {
		t.Fatalf("invited role: %v %q", err, role)
	}
	// A pending account (Google sign-in, not verified) gets no membership yet.
	f.svc.SetMember(ctx, f.owner, "team", "late@x.test", "commenter")
	late, _ := f.svc.SignIn(ctx, "late@x.test", "Late", false)
	if late.IsActive() {
		t.Fatal("unverified account is active")
	}
	if err := f.svc.ManageAccount(ctx, f.admin, late.ID, ActionVerify); err != nil {
		t.Fatal(err)
	}
	late, _ = f.svc.Store().AccountByID(ctx, late.ID)
	if role, _ := f.svc.Role(ctx, Actor{Account: late}, "team"); role != store.ProjectCommenter {
		t.Fatalf("verified invitee role %q", role)
	}
	if err := f.svc.ManageAccount(ctx, f.admin, f.admin.Account.ID, ActionDisable); KindOf(err) != KindForbidden {
		t.Fatalf("admin acting on self: %v", err)
	}
	if err := f.svc.ManageAccount(ctx, f.admin, f.owner.Account.ID, ActionRemove); KindOf(err) != KindConflict {
		t.Fatalf("removing a sole owner: %v", err)
	}
}

func TestRestoreAndHistory(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r1, _ := f.svc.UploadArtifact(ctx, f.owner, "h", "a.html", str("v1"), nil, "", "", "one")
	r2, _ := f.svc.UploadArtifact(ctx, f.owner, "h", "a.html", str("v2"), nil, r1.Files[0].Blob, "", "two")
	f.svc.UploadArtifact(ctx, f.owner, "h", "b.html", str("b"), nil, "", "", "three")
	hist, err := f.svc.History(ctx, f.owner, "h", "a.html", 0, 0)
	if err != nil || len(hist) != 2 || hist[0].Message != "two" {
		t.Fatalf("file history: %v %+v", err, hist)
	}
	res, err := f.svc.Restore(ctx, f.owner, "h", r1.Version, "a.html", "")
	if err != nil || res.Unchanged {
		t.Fatalf("restore file: %v %+v", err, res)
	}
	fc, _ := f.svc.ReadFile(ctx, f.owner, "h", "a.html", "")
	if fc.Content != "v1" {
		t.Fatalf("restored content %q", fc.Content)
	}
	// Restoring the whole project to r2 brings a.html back to v2 and removes b.html.
	if _, err := f.svc.Restore(ctx, f.owner, "h", r2.Version, "", ""); err != nil {
		t.Fatal(err)
	}
	_, files, _ := f.svc.ListFiles(ctx, f.owner, "h", "", "", true)
	if len(files) != 1 || files[0].Path != "a.html" {
		t.Fatalf("after project restore: %+v", files)
	}
	_, _, diff, err := f.svc.Diff(ctx, f.owner, "h", r1.Version, "", "a.html")
	if err != nil || !strings.Contains(diff, "+v2") {
		t.Fatalf("diff: %v %q", err, diff)
	}
}

func TestThreadsFollowMovesAndResolve(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	r1, _ := f.svc.UploadArtifact(ctx, f.owner, "c", "screens/wallet.html", str(`<html><body><h1 id="t">Wallet balance</h1></body></html>`), nil, "", "", "")
	x, y := 10.0, 20.0
	tv, err := f.svc.CreateThread(ctx, Actor{Account: f.owner.Account}, "c", ThreadInput{Path: "screens/wallet.html", Body: "Rename this",
		Anchor: Anchor{Selector: "#t", Text: "Wallet balance", X: &x, Y: &y}})
	if err != nil {
		t.Fatal(err)
	}
	// Move and edit the artifact; the thread follows it.
	r2, err := f.svc.Publish(ctx, f.owner, "c", "", []FileChange{{Path: "screens/wallet.html", MoveTo: "profile/wallet.html", Base: r1.Files[0].Blob,
		Content: str(`<html><body><h1 id="t">Balance</h1><p>a much longer page than before, with more text in it</p></body></html>`)}})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := f.svc.ListThreads(ctx, f.owner, ThreadQuery{Project: "c", Path: "profile/wallet.html"})
	if err != nil || len(ts) != 1 || ts[0].OriginalPath != "screens/wallet.html" {
		t.Fatalf("threads after move: %v %+v", err, ts)
	}
	d, err := f.svc.GetThread(ctx, f.owner, tv.ID)
	if err != nil || !d.ChangedSince || !strings.Contains(d.Excerpt, `id="t"`) {
		t.Fatalf("detail: %v %+v", err, d)
	}
	if _, err := f.svc.Resolve(ctx, f.owner, tv.ID, r2.Version, "Done"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Resolve(ctx, f.owner, tv.ID, "", ""); KindOf(err) != KindConflict {
		t.Fatalf("double resolve: %v", err)
	}
	open, _ := f.svc.ListThreads(ctx, f.owner, ThreadQuery{State: "open"})
	if len(open) != 0 {
		t.Fatalf("open threads: %+v", open)
	}
	v, err := f.svc.Reopen(ctx, f.owner, tv.ID, "")
	if err != nil || v.State != "open" || len(v.Entries) != 3 || v.Entries[1].Version != r2.Version || !v.Entries[1].Agent {
		t.Fatalf("reopen: %v %+v", err, v)
	}
	// Someone outside the project can't see it.
	if _, err := f.svc.GetThread(ctx, f.other, tv.ID); KindOf(err) != KindNotFound {
		t.Fatalf("outsider thread read: %v", err)
	}
}

func TestTokens(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	nt, err := f.svc.CreateToken(ctx, f.owner.Account, "Claude Code")
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.svc.Authenticate(ctx, nt.Secret)
	if err != nil || a.Account.ID != f.owner.Account.ID || a.Name() != "Claude Code for owner" {
		t.Fatalf("authenticate: %v %+v", err, a)
	}
	if err := f.svc.RevokeToken(ctx, f.other, nt.ID); KindOf(err) != KindNotFound {
		t.Fatalf("someone else's revoke: %v", err)
	}
	if err := f.svc.RevokeToken(ctx, f.owner, nt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(ctx, nt.Secret); err == nil {
		t.Fatal("revoked token works")
	}
}

func TestArtifactMeta(t *testing.T) {
	m := ParseArtifactMeta([]byte(`<head><TITLE> Wallet &amp; coins </TITLE><meta content="width=1440" name='artifact'></head>`))
	if m.Title != "Wallet & coins" || m.Width != 1440 {
		t.Fatalf("%+v", m)
	}
	if m := ParseArtifactMeta([]byte(`<title>x</title>`)); m.Width != 0 {
		t.Fatalf("%+v", m)
	}
}
