// Package config reads Artifex's YAML configuration. Secrets can also come
// from the environment, so they never need to be in the file.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Listen is the address the HTTP server binds, e.g. "127.0.0.1:8090".
	Listen string `yaml:"listen"`
	// BaseURL is the public address people and agents use, without a
	// trailing slash. Links, OAuth redirects and the content CSP use it.
	BaseURL string `yaml:"base_url"`
	// DataDir holds app.db and projects/.
	DataDir string `yaml:"data_dir"`
	// TrustProxy reads the client address from X-Forwarded-For, for rate
	// limits. Only set it behind a reverse proxy that overwrites the header.
	TrustProxy bool `yaml:"trust_proxy"`
	// SigningKey signs content addresses and the OAuth state. When empty it
	// comes from ARTIFEX_SIGNING_KEY, else from data_dir/signing.key, which is
	// created on first start.
	SigningKey string `yaml:"signing_key"`

	Google Google `yaml:"google"`
	// Admins become active Admin accounts the first time they sign in.
	Admins []string `yaml:"admins"`
	// DevAuth enables /auth/dev, which signs anyone in by email with no
	// password. For local development only.
	DevAuth bool `yaml:"dev_auth"`

	Limits Limits `yaml:"limits"`
}

type Google struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"` // or ARTIFEX_GOOGLE_CLIENT_SECRET
	// AllowedDomains limits sign-in to these Google Workspace domains.
	// Empty allows any verified Google address; Admin verification is then
	// the only gate.
	AllowedDomains []string `yaml:"allowed_domains"`
}

type Limits struct {
	MaxFileBytes    int64         `yaml:"max_file_bytes"`
	MaxPublishBytes int64         `yaml:"max_publish_bytes"`
	ContentURLTTL   time.Duration `yaml:"content_url_ttl"`
	SessionTTL      time.Duration `yaml:"session_ttl"`
	// RequestsPerMinute is the rate limit per client address on sign-in,
	// MCP and git.
	RequestsPerMinute int `yaml:"requests_per_minute"`
}

// Load reads path, applies defaults and the environment, and validates.
func Load(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.applyDefaults()
	if v := os.Getenv("ARTIFEX_SIGNING_KEY"); v != "" && c.SigningKey == "" {
		c.SigningKey = v
	}
	if v := os.Getenv("ARTIFEX_GOOGLE_CLIENT_SECRET"); v != "" && c.Google.ClientSecret == "" {
		c.Google.ClientSecret = v
	}
	if v := os.Getenv("ARTIFEX_GOOGLE_CLIENT_ID"); v != "" && c.Google.ClientID == "" {
		c.Google.ClientID = v
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8090"
	}
	if c.BaseURL == "" {
		c.BaseURL = "http://" + c.Listen
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Limits.MaxFileBytes == 0 {
		c.Limits.MaxFileBytes = 25 << 20
	}
	if c.Limits.MaxPublishBytes == 0 {
		c.Limits.MaxPublishBytes = 50 << 20
	}
	if c.Limits.ContentURLTTL == 0 {
		c.Limits.ContentURLTTL = 2 * time.Hour
	}
	if c.Limits.SessionTTL == 0 {
		c.Limits.SessionTTL = 30 * 24 * time.Hour
	}
	if c.Limits.RequestsPerMinute == 0 {
		c.Limits.RequestsPerMinute = 300
	}
	for i, a := range c.Admins {
		c.Admins[i] = strings.ToLower(strings.TrimSpace(a))
	}
	for i, d := range c.Google.AllowedDomains {
		c.Google.AllowedDomains[i] = strings.ToLower(strings.TrimSpace(d))
	}
}

// MaxRequestBytes is the largest request body /mcp accepts: one full publish
// as JSON, with binary files in base64 (4/3 larger) and room for the JSON
// around them.
func (l Limits) MaxRequestBytes() int64 { return l.MaxPublishBytes*4/3 + 1<<20 }

func (c *Config) validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("base_url must be an http(s) origin with no path, got %q", c.BaseURL)
	}
	if c.Limits.MaxPublishBytes < c.Limits.MaxFileBytes {
		return errors.New("limits.max_publish_bytes must be at least limits.max_file_bytes")
	}
	return nil
}

// Origin is BaseURL's scheme and host.
func (c *Config) Origin() string { return c.BaseURL }

// Secure reports whether cookies get the Secure flag.
func (c *Config) Secure() bool { return strings.HasPrefix(c.BaseURL, "https://") }

// GoogleEnabled reports whether Google sign-in is configured.
func (c *Config) GoogleEnabled() bool { return c.Google.ClientID != "" && c.Google.ClientSecret != "" }

func (c *Config) ProjectsDir() string { return filepath.Join(c.DataDir, "projects") }
func (c *Config) TrashDir() string    { return filepath.Join(c.DataDir, "trash") }
func (c *Config) DBPath() string      { return filepath.Join(c.DataDir, "app.db") }

// Key returns the signing key, creating data_dir/signing.key when none is
// configured.
func (c *Config) Key() ([]byte, error) {
	if c.SigningKey != "" {
		if len(c.SigningKey) < 32 {
			return nil, errors.New("signing_key must be at least 32 characters")
		}
		return []byte(c.SigningKey), nil
	}
	path := filepath.Join(c.DataDir, "signing.key")
	if data, err := os.ReadFile(path); err == nil {
		k := strings.TrimSpace(string(data))
		if len(k) >= 32 {
			return []byte(k), nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	k := hex.EncodeToString(buf)
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(k+"\n"), 0o600); err != nil {
		return nil, err
	}
	return []byte(k), nil
}

// IsAdminEmail reports whether email is in the bootstrap Admins list.
func (c *Config) IsAdminEmail(email string) bool {
	email = strings.ToLower(email)
	for _, a := range c.Admins {
		if a == email {
			return true
		}
	}
	return false
}

// DomainAllowed reports whether email may sign in with Google.
func (c *Config) DomainAllowed(email string) bool {
	if len(c.Google.AllowedDomains) == 0 {
		return true
	}
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	d := strings.ToLower(email[at+1:])
	for _, a := range c.Google.AllowedDomains {
		if d == a {
			return true
		}
	}
	return false
}
