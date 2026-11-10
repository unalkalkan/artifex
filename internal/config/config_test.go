package config

import (
	"os"
	"path/filepath"
	"testing"
)

// git runs inside each project's folder, so a relative data_dir has to
// become absolute: with "./data", every publish once failed.
func TestDataDirIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	os.WriteFile(p, []byte("data_dir: ./data\nscreenshots:\n  renderer: http://renderer:8091\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.DataDir) {
		t.Fatalf("data_dir %q", c.DataDir)
	}
	if c.Limits.UploadURLTTL == 0 {
		t.Fatal("no upload address lifetime")
	}
	os.WriteFile(p, []byte("screenshots:\n  renderer: renderer:8091\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("a renderer address without a scheme was accepted")
	}
}
