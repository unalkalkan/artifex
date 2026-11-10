// Command artifex runs the Artifex server and its administration commands.
//
//	artifex serve                               run the server
//	artifex admin account add <email> [-name N] [-admin]
//	artifex admin account list
//	artifex admin token create <email> [-label L]
//	artifex admin backup <dir>                  copy the database and bundle every project
//	artifex healthcheck                         exit 0 when the local server answers
//
// Every command reads the configuration from -config, $ARTIFEX_CONFIG, or
// ./config.yaml when it exists.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/unalkalkan/artifex/internal/config"
	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/githttp"
	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/mcpserver"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
	"github.com/unalkalkan/artifex/internal/web"
)

var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `Artifex: agents publish HTML, people view it and comment.

Usage:
  artifex serve [-config FILE]
  artifex admin account add <email> [-name NAME] [-admin] [-config FILE]
  artifex admin account list [-config FILE]
  artifex admin token create <email> [-label LABEL] [-config FILE]
  artifex admin backup <dir> [-config FILE]
  artifex healthcheck [-config FILE]       exit 0 when the server answers
  artifex version
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "admin":
		err = admin(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "healthcheck":
		err = healthcheck(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "artifex:", err)
		os.Exit(1)
	}
}

func configPath(fs *flag.FlagSet, args []string) (string, []string, error) {
	p := fs.String("config", "", "configuration file")
	// Flags may come after positional arguments: pull them out first.
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return "", nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	path := *p
	if path == "" {
		path = os.Getenv("ARTIFEX_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}
	return path, pos, nil
}

func newLogger() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.DisableStacktrace = true
	l, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	return l
}

type app struct {
	cfg   *config.Config
	store *store.Store
	svc   *service.Service
	log   *zap.Logger
}

func open(path string, log *zap.Logger) (*app, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{cfg.DataDir, cfg.ProjectsDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	return &app{cfg: cfg, store: st, svc: service.New(cfg, st, events.NewHub(), log), log: log}, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path, _, err := configPath(fs, args)
	if err != nil {
		return err
	}
	log := newLogger()
	defer log.Sync()
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is not installed; Artifex needs it")
	}
	a, err := open(path, log)
	if err != nil {
		return err
	}
	defer a.store.Close()
	key, err := a.cfg.Key()
	if err != nil {
		return err
	}
	if a.cfg.DevAuth {
		log.Warn("dev_auth is on: anyone who can reach this server can sign in by email with no password")
	}
	if !a.cfg.GoogleEnabled() && !a.cfg.DevAuth {
		log.Warn("no sign-in method is configured; set google.client_id and google.client_secret")
	}
	syncWorktrees(a)

	ws, err := web.New(a.svc, key, log)
	if err != nil {
		return err
	}
	gh, err := githttp.New(a.svc, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              a.cfg.Listen,
		Handler:           ws.Handler(web.Options{MCP: mcpserver.Handler(a.svc, log), Git: gh}),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.store.PurgeExpiredSessions(context.Background())
			}
		}
	}()
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", zap.String("addr", a.cfg.Listen), zap.String("base_url", a.cfg.BaseURL), zap.String("version", version))
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// syncWorktrees makes every project's working tree equal to its branch, in
// case the server stopped between a commit and the sync.
func syncWorktrees(a *app) {
	entries, err := os.ReadDir(a.cfg.ProjectsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := gitrepo.Open(filepath.Join(a.cfg.ProjectsDir(), e.Name())).SyncWorktree(context.Background()); err != nil {
			a.log.Warn("working tree not synced", zap.String("project", e.Name()), zap.Error(err))
		}
	}
}

func admin(args []string) error {
	if len(args) < 1 {
		usage()
	}
	log := zap.NewNop()
	ctx := context.Background()
	switch args[0] {
	case "account":
		if len(args) < 2 {
			usage()
		}
		fs := flag.NewFlagSet("account", flag.ExitOnError)
		name := fs.String("name", "", "display name")
		isAdmin := fs.Bool("admin", false, "make the account an Admin")
		path, pos, err := configPath(fs, args[2:])
		if err != nil {
			return err
		}
		a, err := open(path, log)
		if err != nil {
			return err
		}
		defer a.store.Close()
		switch args[1] {
		case "add":
			if len(pos) != 1 {
				usage()
			}
			acct, err := a.svc.AddAccount(ctx, pos[0], *name, *isAdmin)
			if err != nil {
				return err
			}
			fmt.Printf("%s is an active %s (account %d)\n", acct.Email, acct.Role, acct.ID)
		case "list":
			accts, err := a.store.ListAccounts(ctx)
			if err != nil {
				return err
			}
			for _, ac := range accts {
				fmt.Printf("%-5d %-9s %-7s %s %s\n", ac.ID, ac.Status, ac.Role, ac.Email, ac.Name)
			}
		default:
			usage()
		}
	case "token":
		if len(args) < 2 || args[1] != "create" {
			usage()
		}
		fs := flag.NewFlagSet("token", flag.ExitOnError)
		label := fs.String("label", "Claude", "label shown with the account's name")
		path, pos, err := configPath(fs, args[2:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			usage()
		}
		a, err := open(path, log)
		if err != nil {
			return err
		}
		defer a.store.Close()
		acct, err := a.store.AccountByEmail(ctx, pos[0])
		if err != nil {
			return fmt.Errorf("no account %s; add it with: artifex admin account add %s", pos[0], pos[0])
		}
		nt, err := a.svc.CreateToken(ctx, acct, *label)
		if err != nil {
			return err
		}
		fmt.Println(nt.Secret)
		fmt.Fprintf(os.Stderr, "Token %q for %s. It is shown only once.\nClaude Code: claude mcp add --transport http artifex %s/mcp --header \"Authorization: Bearer %s\"\n", nt.Label, acct.Email, a.cfg.BaseURL, nt.Secret)
	case "backup":
		fs := flag.NewFlagSet("backup", flag.ExitOnError)
		path, pos, err := configPath(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			usage()
		}
		a, err := open(path, log)
		if err != nil {
			return err
		}
		defer a.store.Close()
		return backup(ctx, a, pos[0])
	default:
		usage()
	}
	return nil
}

// backup writes a consistent copy of the database and a git bundle of every
// project into a new folder under dir.
func backup(ctx context.Context, a *app, dir string) error {
	out := filepath.Join(dir, "artifex-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(filepath.Join(out, "projects"), 0o750); err != nil {
		return err
	}
	if err := a.store.Backup(ctx, filepath.Join(out, "app.db")); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	entries, err := os.ReadDir(a.cfg.ProjectsDir())
	if err != nil {
		return err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repo := gitrepo.Open(filepath.Join(a.cfg.ProjectsDir(), e.Name()))
		if h, _ := repo.Head(ctx); h == "" {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "bundle", "create", filepath.Join(out, "projects", e.Name()+".bundle"), "--all")
		cmd.Dir = repo.Dir
		cmd.Env = gitrepo.Env()
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", e.Name(), err, strings.TrimSpace(string(b)))
		}
		n++
	}
	fmt.Printf("backed up the database and %d projects to %s\n", n, out)
	return nil
}

// healthcheck asks the local server for /healthz, for the container's
// HEALTHCHECK.
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	path, _, err := configPath(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	addr := cfg.Listen
	if strings.HasPrefix(addr, "0.0.0.0:") || strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1:" + addr[strings.LastIndexByte(addr, ':')+1:]
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %d", resp.StatusCode)
	}
	return nil
}
