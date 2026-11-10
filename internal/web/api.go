package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

func (s *Server) apiThreads(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	q := service.ThreadQuery{Project: project, Path: r.URL.Query().Get("path"), State: r.URL.Query().Get("state"), Limit: 500}
	ts, err := s.svc.ListThreads(r.Context(), a, q)
	if err != nil {
		s.jsonError(w, err)
		return
	}
	if ts == nil {
		ts = []service.ThreadView{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"threads": ts})
}

type createThreadBody struct {
	Path    string         `json:"path"`
	Version string         `json:"version"`
	Anchor  service.Anchor `json:"anchor"`
	Body    string         `json:"body"`
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return &service.Error{Kind: service.KindInvalid, Msg: "the request body is not valid JSON"}
	}
	return nil
}

func (s *Server) apiCreateThread(w http.ResponseWriter, r *http.Request, a service.Actor) {
	var b createThreadBody
	if err := decode(r, &b); err != nil {
		s.jsonError(w, err)
		return
	}
	tv, err := s.svc.CreateThread(r.Context(), a, r.PathValue("project"), service.ThreadInput{Path: b.Path, Version: b.Version, Anchor: b.Anchor, Body: b.Body})
	if err != nil {
		s.jsonError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, tv)
}

func (s *Server) apiThreadAction(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.jsonError(w, &service.Error{Kind: service.KindNotFound, Msg: "thread not found"})
		return
	}
	var b struct {
		Body    string `json:"body"`
		Version string `json:"version"`
	}
	if err := decode(r, &b); err != nil {
		s.jsonError(w, err)
		return
	}
	var tv service.ThreadView
	switch r.PathValue("action") {
	case "reply":
		tv, err = s.svc.Reply(r.Context(), a, id, b.Body)
	case "resolve":
		tv, err = s.svc.Resolve(r.Context(), a, id, b.Version, b.Body)
	case "reopen":
		tv, err = s.svc.Reopen(r.Context(), a, id, b.Body)
	default:
		err = &service.Error{Kind: service.KindNotFound, Msg: "unknown action"}
	}
	if err != nil {
		s.jsonError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, tv)
}

// apiFrame returns a fresh signed address for an artifact at a version (the
// latest by default), for reloading after a publish or when one expires.
func (s *Server) apiFrame(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	ctx := r.Context()
	if _, err := s.svc.Require(ctx, a, project, store.ProjectCommenter); err != nil {
		s.jsonError(w, err)
		return
	}
	commit, err := s.svc.ResolveVersion(ctx, project, r.URL.Query().Get("v"))
	if err != nil {
		s.jsonError(w, err)
		return
	}
	p, err := service.CleanPath(r.URL.Query().Get("path"))
	if err != nil {
		s.jsonError(w, err)
		return
	}
	e, err := s.svc.Entry(ctx, project, commit, p)
	if err != nil {
		s.jsonError(w, err)
		return
	}
	out := map[string]any{"url": s.content.URL(project, commit, p), "version": commit, "blob": e.ID}
	if service.IsHTML(p) {
		m := s.svc.ArtifactMetaOf(ctx, project, e.ID, path.Base(p))
		out["title"], out["width"] = m.Title, m.Width
	}
	s.writeJSON(w, http.StatusOK, out)
}

// apiEvents streams the project's changes to an open page (design decision 21).
func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	if _, err := s.svc.Require(r.Context(), a, project, store.ProjectCommenter); err != nil {
		s.jsonError(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	ch, cancel := s.svc.Hub().Subscribe(project)
	defer cancel()
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	// A session can end while a page stays open; check it now and then.
	recheck := time.NewTicker(5 * time.Minute)
	defer recheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// Membership can end while a page stays open.
			if _, err := s.svc.Require(r.Context(), a, project, store.ProjectCommenter); err != nil {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-recheck.C:
			sess := s.loadSession(r)
			if sess == nil {
				return
			}
			a = service.Actor{Account: sess.account}
			if _, err := s.svc.Require(r.Context(), a, project, store.ProjectCommenter); err != nil {
				return
			}
		}
	}
}
