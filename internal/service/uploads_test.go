package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"
)

// tarball builds a gzipped tar: files by path, plus a symlink, a folder and
// macOS metadata, which are skipped.
func tarball(t *testing.T, files map[string]string, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, name := range order {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "./link.html", Typeflag: tar.TypeSymlink, Linkname: "index.html"})
	tw.WriteHeader(&tar.Header{Name: "./._index.html", Typeflag: tar.TypeReg, Size: 0})
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestUploadsThenPublish(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.svc.CreateProject(ctx, f.owner, "up", ""); err != nil {
		t.Fatal(err)
	}
	// One file.
	res, err := f.svc.StoreUpload(ctx, f.owner, "up", "./assets/app.css", strings.NewReader("h1{color:teal}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "assets/app.css" || len(res.Changes[0].Upload) != 40 || res.Bytes != 14 {
		t.Fatalf("upload: %+v", res)
	}
	css := res.Changes[0]
	// A folder, as a tar.
	arch, err := f.svc.StoreArchive(ctx, f.owner, "up", bytes.NewReader(tarball(t, map[string]string{
		"./index.html":     `<!doctype html><title>Home</title><link rel="stylesheet" href="assets/app.css"><h1>Hi</h1>`,
		"./img/logo.svg":   `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"./.git/config":    "x",
		"./img/logo2.svg":  "first",
		"img/logo2.svg":    "second",
		"./-/anything.txt": "x",
	}, []string{"./index.html", "./img/logo.svg", "./.git/config", "./img/logo2.svg", "img/logo2.svg", "./-/anything.txt"})))
	if err != nil {
		t.Fatal(err)
	}
	if len(arch.Changes) != 3 || arch.Changes[0].Path != "index.html" || arch.Changes[2].Path != "img/logo2.svg" {
		t.Fatalf("archive: %+v", arch)
	}
	if len(arch.Skipped) != 4 { // .git, the "-" folder, the symlink, the metadata
		t.Fatalf("skipped: %v", arch.Skipped)
	}
	// Publish by upload id: the content never travels again.
	changes := []FileChange{{Path: css.Path, Upload: css.Upload}}
	for _, u := range arch.Changes {
		changes = append(changes, FileChange{Path: u.Path, Upload: u.Upload})
	}
	pub, err := f.svc.PublishOrCreate(ctx, f.owner, "up", "", "From disk", changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Warnings) != 0 {
		t.Fatalf("warnings: %v", pub.Warnings)
	}
	got, err := f.svc.ReadFile(ctx, f.owner, "up", "img/logo2.svg", "")
	if err != nil || got.Content != "second" {
		t.Fatalf("later entry should win: %+v %v", got, err)
	}
	// Replacing a file needs its base, as with content; the conflict carries
	// the current blob, not the content: the sender works from its disk.
	again, _ := f.svc.StoreUpload(ctx, f.owner, "up", "index.html", strings.NewReader("<title>New</title>"))
	_, err = f.svc.PublishOrCreate(ctx, f.owner, "up", "", "", []FileChange{{Path: "index.html", Upload: again.Changes[0].Upload}})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Files[0].Current == "" || ce.Files[0].Content != "" {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := f.svc.PublishOrCreate(ctx, f.owner, "up", "", "", []FileChange{{Path: "index.html", Upload: again.Changes[0].Upload, Base: ce.Files[0].Current}}); err != nil {
		t.Fatal(err)
	}
	// Ids that aren't this project's blobs, and mixed content.
	for _, ch := range []FileChange{
		{Path: "x.html", Upload: strings.Repeat("a", 40)},
		{Path: "x.html", Upload: "abc"},
		{Path: "x.html", Upload: css.Upload, Content: str("x")},
		{Path: "x.html", Upload: pub.Version}, // a commit, not a blob
	} {
		if _, err := f.svc.PublishOrCreate(ctx, f.owner, "up", "", "", []FileChange{ch}); KindOf(err) != KindInvalid {
			t.Fatalf("%+v: %v", ch, err)
		}
	}
	// Too large, and not the editor role.
	big := strings.Repeat("a", int(f.svc.cfg.Limits.MaxFileBytes)+1)
	if _, err := f.svc.StoreUpload(ctx, f.owner, "up", "big.txt", strings.NewReader(big)); KindOf(err) != KindInvalid || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("too large: %v", err)
	}
	if _, err := f.svc.StoreArchive(ctx, f.owner, "up", bytes.NewReader(tarball(t, map[string]string{"big.txt": big}, []string{"big.txt"}))); KindOf(err) != KindInvalid {
		t.Fatalf("too large in an archive: %v", err)
	}
	if _, err := f.svc.StoreArchive(ctx, f.owner, "up", strings.NewReader("not a tar")); KindOf(err) != KindInvalid {
		t.Fatalf("not a tar: %v", err)
	}
	if _, err := f.svc.StoreUpload(ctx, f.other, "up", "a.css", strings.NewReader("x")); KindOf(err) != KindNotFound {
		t.Fatalf("non-member: %v", err)
	}
}

func TestPublishCreatesProject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// A publish that can't succeed creates nothing.
	if _, err := f.svc.PublishOrCreate(ctx, f.owner, "fresh", "Fresh", "", []FileChange{{Path: "../x.html", Content: str("x")}}); KindOf(err) != KindInvalid {
		t.Fatalf("bad path: %v", err)
	}
	if _, err := f.svc.Project(ctx, f.owner, "fresh"); KindOf(err) != KindNotFound {
		t.Fatalf("created anyway: %v", err)
	}
	res, err := f.svc.PublishOrCreate(ctx, f.owner, "fresh", "Fresh", "", []FileChange{{Path: "index.html", Content: str("<title>Hi</title>")}})
	if err != nil || !res.ProjectCreated {
		t.Fatalf("publish: %+v %v", res, err)
	}
	if p, _ := f.svc.Project(ctx, f.owner, "fresh"); p.Title != "Fresh" || p.Role != "owner" {
		t.Fatalf("project: %+v", p)
	}
	// When the publish writes project.yaml itself, the title doesn't clash with it.
	res, err = f.svc.PublishOrCreate(ctx, f.owner, "own-yaml", "Ignored", "", []FileChange{{Path: "project.yaml", Content: str("title: Mine\n")}})
	if err != nil || !res.ProjectCreated {
		t.Fatalf("own project.yaml: %v", err)
	}
	if p, _ := f.svc.Project(ctx, f.owner, "own-yaml"); p.Title != "Mine" {
		t.Fatalf("title: %q", p.Title)
	}
	// Someone else's project id is taken, without saying more.
	if _, err := f.svc.PublishOrCreate(ctx, f.other, "fresh", "", "", []FileChange{{Path: "b.html", Content: str("b")}}); KindOf(err) != KindConflict {
		t.Fatalf("taken: %v", err)
	}
}

func TestOrderAndHeight(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.svc.PublishOrCreate(ctx, f.owner, "ord", "", "", []FileChange{
		{Path: "project.yaml", Content: str("order: [system, game/, zeta, nothing]\n")},
		{Path: "alpha.html", Content: str("<title>A</title>")},
		{Path: "zeta.html", Content: str(`<title>Z</title><meta name="artifact" content="width=1440, height=900">`)},
		{Path: "game/folder.yaml", Content: str("order: [sub, b]\n")},
		{Path: "game/a.html", Content: str("a")},
		{Path: "game/b.html", Content: str("b")},
		{Path: "game/sub/x.html", Content: str("x")},
		{Path: "game/other/x.html", Content: str("x")},
		{Path: "live-ops/x.html", Content: str("x")},
		{Path: "system/x.html", Content: str("x")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Warnings, "|") != `project.yaml lists "nothing", which doesn't exist` {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	names := func(dir string) (folders, arts []string) {
		fo, err := f.svc.FolderAt(ctx, "ord", res.Version, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range fo.Folders {
			folders = append(folders, x.Path)
		}
		for _, x := range fo.Artifacts {
			arts = append(arts, x.Name)
		}
		return
	}
	if fo, ar := names(""); strings.Join(fo, ",") != "system,game,live-ops" || strings.Join(ar, ",") != "zeta.html,alpha.html" {
		t.Fatalf("root: %v %v", fo, ar)
	}
	if fo, ar := names("game"); strings.Join(fo, ",") != "game/sub,game/other" || strings.Join(ar, ",") != "b.html,a.html" {
		t.Fatalf("game: %v %v", fo, ar)
	}
	fo, _ := f.svc.FolderAt(ctx, "ord", res.Version, "")
	if z := fo.Artifacts[0]; z.Width != 1440 || z.Height != 900 {
		t.Fatalf("size: %+v", z)
	}
	for in, want := range map[string][2]int{
		`<meta name="artifact" content="width=390,height=844">`:  {390, 844},
		`<meta content="height=700" name="artifact">`:            {0, 700},
		`<meta name="artifact" content="width=1440, height=20">`: {1440, 0},
		`<meta name="viewport" content="width=device-width">`:    {0, 0},
	} {
		if m := ParseArtifactMeta([]byte(in)); m.Width != want[0] || m.Height != want[1] {
			t.Fatalf("%s: %+v", in, m)
		}
	}
}

func TestWarningsForWhatBreaksWhenViewed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	res, err := f.svc.PublishOrCreate(ctx, f.owner, "warn", "", "", []FileChange{
		{Path: "s/page.html", Content: str(`<!doctype html><title>P</title>
<link rel="stylesheet" href="https://cdn.example.com/tabler.css">
<link rel="preconnect" href="https://fonts.example.com">
<link rel="stylesheet" href="../app.css">
<script src="//cdn.example.com/react.js"></script>
<img srcset="a.png 1x, https://img.example.com/b.png 2x" src="data:image/png;base64,AA==">
<a href="https://example.com/docs">docs</a>
<img src="/assets/logo.png">
<style>.x{background:url('bg.png')} @import "https://fonts.example.com/css";</style>
<div style="background-image:url(&quot;https://img.example.com/c.png&quot;)"></div>
<svg><rect fill="url(#g)"/></svg>`)},
		{Path: "app.css", Content: str(`@import url("fonts/inter.css"); .a{background:url(data:image/svg+xml;utf8,<svg/>)} .b{background:url("img/missing.png")} .c{background:url(https://cdn.example.com/x.png)}`)},
		{Path: "fonts/inter.css", Content: str(`@font-face{src:url(inter.woff2)}`)},
		{Path: "fonts/inter.woff2", ContentBase64: str("AAAA")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(res.Warnings, "\n")
	for _, want := range []string{
		"s/page.html: https://cdn.example.com/tabler.css is on another host",
		"s/page.html: //cdn.example.com/react.js is on another host",
		"s/page.html: https://img.example.com/b.png is on another host",
		"s/page.html: https://fonts.example.com/css is on another host",
		"s/page.html: https://img.example.com/c.png is on another host",
		"s/page.html: a.png doesn't exist",
		"s/page.html: bg.png doesn't exist",
		"s/page.html: /assets/logo.png starts with /",
		"app.css: img/missing.png doesn't exist",
		"app.css: https://cdn.example.com/x.png is on another host",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing warning %q in:\n%s", want, got)
		}
	}
	for _, not := range []string{"preconnect", "fonts.example.com is", "example.com/docs", "#g", "data:", "&quot;", "app.css doesn't", "inter"} {
		if strings.Contains(got, not) {
			t.Errorf("unexpected warning about %q in:\n%s", not, got)
		}
	}
	if len(res.Warnings) != 10 {
		t.Errorf("%d warnings:\n%s", len(res.Warnings), got)
	}
}
