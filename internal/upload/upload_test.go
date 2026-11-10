package upload

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

func TestUploadAddresses(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewUnstartedServer(nil)
	cfg := &config.Config{BaseURL: "http://" + srv.Listener.Addr().String(), DataDir: dir}
	cfg.Limits.MaxFileBytes, cfg.Limits.MaxPublishBytes, cfg.Limits.UploadURLTTL = 1<<20, 2<<20, time.Minute
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := service.New(cfg, st, events.NewHub(), zap.NewNop())
	up := New(svc, []byte("0123456789abcdef0123456789abcdef"), zap.NewNop())
	srv.Config.Handler = up
	srv.Start()
	defer srv.Close()

	ctx := context.Background()
	acct, _ := svc.AddAccount(ctx, "dev@x.test", "Dev", false)
	tok, _ := svc.CreateToken(ctx, acct, "Claude")
	agent, err := svc.Authenticate(ctx, tok.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateProject(ctx, agent, "site", ""); err != nil {
		t.Fatal(err)
	}
	addr := up.Create(agent, "site")
	if !strings.HasSuffix(addr.URL, "/site/") || addr.ArchiveURL != addr.URL+"-/archive" || len(addr.Examples) != 2 {
		t.Fatalf("address: %+v", addr)
	}

	put := func(u string, body io.Reader) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPut, u, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// One file, then publish by its id.
	code, out := put(addr.URL+"screens/home.html", strings.NewReader("<title>Home</title>"))
	if code != http.StatusCreated {
		t.Fatalf("put: %d %v", code, out)
	}
	ch := out["changes"].([]any)[0].(map[string]any)
	if ch["path"] != "screens/home.html" || out["bytes"].(float64) != 19 {
		t.Fatalf("answer: %v", out)
	}
	res, err := svc.Publish(ctx, agent, "site", "", []service.FileChange{{Path: ch["path"].(string), Upload: ch["upload"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := svc.ReadFile(ctx, agent, "site", "screens/home.html", res.Version); f.Content != "<title>Home</title>" {
		t.Fatalf("published: %+v", f)
	}

	// A folder as one tar.
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for name, body := range map[string]string{"a.css": "a{}", "img/b.svg": "<svg/>"} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	if code, out := put(addr.ArchiveURL, &tb); code != http.StatusCreated || len(out["changes"].([]any)) != 2 {
		t.Fatalf("archive: %d %v", code, out)
	}

	// Refusals.
	for name, c := range map[string]struct {
		url  string
		body string
		want int
	}{
		"no path":         {addr.URL, "x", http.StatusBadRequest},
		"a folder":        {addr.URL + "screens/", "x", http.StatusBadRequest},
		"bad path":        {addr.URL + ".git/config", "x", http.StatusBadRequest},
		"another project": {strings.Replace(addr.URL, "/site/", "/other/", 1) + "a.css", "x", http.StatusForbidden},
		"tampered":        {strings.Replace(addr.URL, "/u/", "/u/1", 1) + "a.css", "x", http.StatusForbidden},
		"too large":       {addr.URL + "big.bin", strings.Repeat("a", 1<<20+1), http.StatusRequestEntityTooLarge},
	} {
		if code, out := put(c.url, strings.NewReader(c.body)); code != c.want {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if resp, _ := http.Get(addr.URL + "a.css"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}

	// An expired address.
	cfg.Limits.UploadURLTTL = -time.Second
	old := up.Create(agent, "site")
	cfg.Limits.UploadURLTTL = time.Minute
	if code, _ := put(old.URL+"a.css", strings.NewReader("x")); code != http.StatusForbidden {
		t.Errorf("expired: %d", code)
	}
	// A commenter's address stores nothing.
	other, _ := svc.AddAccount(ctx, "c@x.test", "C", false)
	svc.SetMember(ctx, agent, "site", "c@x.test", "commenter")
	if code, _ := put(up.Create(service.Actor{Account: other}, "site").URL+"a.css", strings.NewReader("x")); code != http.StatusForbidden {
		t.Errorf("commenter: %d", code)
	}
	// Revoking the token stops its addresses at once.
	if err := svc.RevokeToken(ctx, agent, agent.TokenID); err != nil {
		t.Fatal(err)
	}
	if code, out := put(addr.URL+"a.css", strings.NewReader("x")); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "revoked") {
		t.Errorf("revoked: %d %v", code, out)
	}
}
