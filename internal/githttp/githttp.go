// Package githttp lets agents with a shell clone and push projects over
// HTTPS (design phase 3), through `git http-backend`:
//
//	git clone https://token:<token>@<host>/git/<project>.git
//
// The password is an Artifex token; the user name is ignored. Fetching needs
// the Commenter role, pushing the Editor role. Each repository refuses
// force-pushes and branch deletions (receive.denyNonFastForwards,
// receive.denyDeletes), so no version can be lost through a push.
package githttp

import (
	"net/http"
	"net/http/cgi"
	"os/exec"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/events"
	"github.com/unalkalkan/artifex/internal/gitrepo"
	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

type Handler struct {
	svc *service.Service
	log *zap.Logger
	git string
}

func New(svc *service.Service, log *zap.Logger) (*Handler, error) {
	g, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	return &Handler{svc: svc, log: log, git: g}, nil
}

// maxPush is the largest pack a push may send: four publishes' worth.
func (h *Handler) maxPush() int64 { return 4 * h.svc.Config().Limits.MaxPublishBytes }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	i := strings.Index(rest, ".git/")
	if i <= 0 {
		http.NotFound(w, r)
		return
	}
	project, tail := rest[:i], rest[i+4:]
	push := tail == "/git-receive-pack" || (tail == "/info/refs" && r.URL.Query().Get("service") == "git-receive-pack")
	switch tail {
	case "/info/refs", "/git-upload-pack", "/git-receive-pack":
	default:
		http.NotFound(w, r) // smart HTTP only
		return
	}
	_, pass, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Artifex", charset="UTF-8"`)
		http.Error(w, "an Artifex token is required as the password", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	a, err := h.svc.Authenticate(ctx, pass)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="Artifex", charset="UTF-8"`)
		http.Error(w, "the token is not valid", http.StatusUnauthorized)
		return
	}
	min := store.ProjectCommenter
	if push {
		min = store.ProjectEditor
	}
	if _, err := h.svc.Require(ctx, a, project, min); err != nil {
		status := http.StatusNotFound
		if service.KindOf(err) == service.KindForbidden {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	repo := h.svc.Repo(project)
	var before string
	if tail == "/git-receive-pack" {
		// One writer at a time per project, shared with publishes.
		unlock := h.svc.Lock(project)
		defer unlock()
		before, _ = repo.Head(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, h.maxPush())
	}
	env := append(gitrepo.Env(),
		"GIT_PROJECT_ROOT="+h.svc.Config().ProjectsDir(),
		"GIT_HTTP_EXPORT_ALL=1",
		"REMOTE_USER="+a.Account.Email, // enables receive-pack
		"GIT_COMMITTER_NAME="+a.Name(),
		"GIT_COMMITTER_EMAIL="+a.Account.Email,
		// Check every pushed object, and refuse packs larger than a few publishes.
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=receive.fsckObjects", "GIT_CONFIG_VALUE_0=true",
		"GIT_CONFIG_KEY_1=receive.maxInputSize", "GIT_CONFIG_VALUE_1="+strconv.FormatInt(h.maxPush(), 10),
		"GIT_CONFIG_KEY_2=transfer.fsckObjects", "GIT_CONFIG_VALUE_2=true",
	)
	cg := &cgi.Handler{Path: h.git, Args: []string{"http-backend"}, Root: "/git", Env: env, Logger: nil}
	r2 := r.Clone(ctx)
	r2.URL.Path = "/git/" + project + tail // http-backend finds <project>/.git
	cg.ServeHTTP(w, r2)

	if tail == "/git-receive-pack" {
		after, _ := repo.Head(ctx)
		if after != "" && after != before {
			var paths []string
			if changes, err := repo.Changes(ctx, before, after); err == nil {
				for _, c := range changes {
					paths = append(paths, c.Path)
					if c.OldPath != "" {
						paths = append(paths, c.OldPath)
					}
				}
			}
			h.svc.Hub().Publish(events.Event{Type: "version", Project: project, Version: after, Paths: paths})
			h.log.Info("pushed", zap.String("project", project), zap.String("version", after[:10]), zap.String("by", a.Name()))
		}
	}
}
