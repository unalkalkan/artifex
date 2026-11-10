package web

import (
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/unalkalkan/artifex/internal/service"
	"github.com/unalkalkan/artifex/internal/store"
)

func (s *Server) home(w http.ResponseWriter, r *http.Request, a service.Actor) {
	ps, err := s.svc.ListProjects(r.Context(), a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var mine, others []service.ProjectView
	for _, p := range ps {
		if p.Role == "" {
			others = append(others, p)
		} else {
			mine = append(mine, p)
		}
	}
	s.render(w, r, http.StatusOK, "home", pageData{Title: "Projects", Data: map[string]any{
		"Projects": mine, "Others": others, "MCP": s.svc.Config().BaseURL + "/mcp",
	}})
}

// projectCtx is what every project page shows in its header.
type projectCtx struct {
	Project   service.ProjectView
	Role      string // the actor's effective role
	Version   string // the version shown
	Latest    string // the latest version
	IsLatest  bool
	CanEdit   bool // Editor or above: restore
	IsOwner   bool
	VersionAt *service.Version
}

func (s *Server) projectContext(r *http.Request, a service.Actor, project string) (projectCtx, error) {
	ctx := r.Context()
	pv, err := s.svc.Project(ctx, a, project)
	if err != nil {
		return projectCtx{}, err
	}
	role, _ := s.svc.Role(ctx, a, project)
	pc := projectCtx{Project: pv, Role: role, Latest: pv.Version, CanEdit: store.RoleRank(role) >= store.RoleRank(store.ProjectEditor), IsOwner: role == store.ProjectOwner}
	v := r.URL.Query().Get("v")
	if v == "" {
		pc.Version, pc.IsLatest = pv.Version, true
		return pc, nil
	}
	c, err := s.svc.ResolveVersion(ctx, project, v)
	if err != nil {
		return pc, err
	}
	pc.Version, pc.IsLatest = c, c == pv.Version
	if !pc.IsLatest {
		if vi, err := s.svc.VersionInfo(ctx, project, c); err == nil {
			pc.VersionAt = &vi
		}
	}
	return pc, nil
}

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	pc, err := s.projectContext(r, a, project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := map[string]any{"P": pc}
	ctx := r.Context()
	if pc.Version != "" {
		f, err := s.svc.FolderAt(ctx, project, pc.Version, "")
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data["Folder"] = f
		open := pc.Project.Open
		if open == "" {
			for _, art := range f.Artifacts {
				if art.Name == "index.html" {
					open = art.Path
				}
			}
		}
		data["Open"] = open
		hist, _ := s.svc.History(ctx, a, project, "", 6, 0)
		data["History"] = hist
		data["Counts"] = s.openCounts(r, a, project)
	}
	s.render(w, r, http.StatusOK, "project", pageData{Title: pc.Project.Title, Data: data})
}

// openCounts counts open threads per artifact path.
func (s *Server) openCounts(r *http.Request, a service.Actor, project string) map[string]int {
	ts, err := s.svc.ListThreads(r.Context(), a, service.ThreadQuery{Project: project, State: store.ThreadOpen, Limit: 500})
	counts := map[string]int{}
	if err != nil {
		return counts
	}
	for _, t := range ts {
		counts[t.Path]++
	}
	return counts
}

// viewPage shows a folder as a canvas, an HTML file as an artifact, and any
// other file as itself.
func (s *Server) viewPage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	raw := strings.TrimSuffix(r.PathValue("path"), "/")
	pc, err := s.projectContext(r, a, project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if pc.Version == "" {
		http.Redirect(w, r, "/p/"+url.PathEscape(project), http.StatusSeeOther)
		return
	}
	p, err := service.CleanDir(raw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	e, err := s.svc.Entry(ctx, project, pc.Version, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if e.Type == "tree" {
		s.canvasPage(w, r, a, pc, p)
		return
	}
	folder, _ := s.svc.FolderAt(ctx, project, pc.Version, service.Parent(p))
	if !service.IsHTML(p) {
		s.render(w, r, http.StatusOK, "file", pageData{Title: path.Base(p), Wide: true, Data: map[string]any{
			"P": pc, "Path": p, "Size": e.Size, "URL": s.content.URL(project, pc.Version, p), "Folder": folder,
		}})
		return
	}
	meta := s.svc.ArtifactMetaOf(ctx, project, e.ID, path.Base(p))
	var prev, next string
	for i, art := range folder.Artifacts {
		if art.Path == p {
			if i > 0 {
				prev = folder.Artifacts[i-1].Path
			}
			if i+1 < len(folder.Artifacts) {
				next = folder.Artifacts[i+1].Path
			}
		}
	}
	s.render(w, r, http.StatusOK, "artifact", pageData{Title: meta.Title, Wide: true, Data: map[string]any{
		"P": pc, "Path": p, "Meta": meta, "Folder": folder, "Prev": prev, "Next": next,
		"Frame": map[string]any{
			"project": project, "path": p, "version": pc.Version, "latest": pc.IsLatest, "url": s.content.URL(project, pc.Version, p),
			"width": meta.Width, "height": meta.Height, "title": meta.Title, "canComment": true, "label": folder.Meta.Label,
		},
	}})
}

func (s *Server) canvasPage(w http.ResponseWriter, r *http.Request, a service.Actor, pc projectCtx, dir string) {
	f, err := s.svc.FolderAt(r.Context(), pc.Project.ID, pc.Version, dir)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	counts := s.openCounts(r, a, pc.Project.ID)
	type frame struct {
		Path, Title, URL string
		Width, Height    int
		Open             int
	}
	var frames []frame
	for _, art := range f.Artifacts {
		frames = append(frames, frame{Path: art.Path, Title: art.Title, URL: s.content.URL(pc.Project.ID, pc.Version, art.Path), Width: art.Width, Height: art.Height, Open: counts[art.Path]})
	}
	title := f.Meta.Title
	if title == "" {
		title = path.Base(dir)
		if dir == "" {
			title = pc.Project.Title
		}
	}
	s.render(w, r, http.StatusOK, "canvas", pageData{Title: title, Wide: true, Data: map[string]any{
		"P": pc, "Dir": dir, "Folder": f, "Frames": frames, "FolderTitle": title,
		"Config": map[string]any{"project": pc.Project.ID, "dir": dir, "version": pc.Version, "latest": pc.IsLatest},
	}})
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	pc, err := s.projectContext(r, a, project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := r.URL.Query().Get("path")
	if p != "" {
		if p, err = service.CleanPath(p); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 0 {
		page = 0
	}
	const per = 50
	vs, err := s.svc.History(r.Context(), a, project, p, per+1, page*per)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	more := len(vs) > per
	if more {
		vs = vs[:per]
	}
	s.render(w, r, http.StatusOK, "history", pageData{Title: "History", Notice: r.URL.Query().Get("notice"), Data: map[string]any{
		"P": pc, "Path": p, "Versions": vs, "Page": page, "More": more, "IsHTML": service.IsHTML(p),
	}})
}

func (s *Server) comparePage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	pc, err := s.projectContext(r, a, project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	p := q.Get("path")
	if p != "" {
		if p, err = service.CleanPath(p); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	ctx := r.Context()
	from, to, diff, err := s.svc.Diff(ctx, a, project, q.Get("from"), q.Get("to"), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := map[string]any{"P": pc, "Path": p, "From": from, "To": to, "Diff": diffLines(diff)}
	if fv, err := s.svc.VersionInfo(ctx, project, from); err == nil {
		data["FromV"] = fv
	}
	if tv, err := s.svc.VersionInfo(ctx, project, to); err == nil {
		data["ToV"] = tv
	}
	if p != "" && service.IsHTML(p) {
		// The artifact may have had another path at the older version.
		oldPath := p
		if from != "" {
			if _, err := s.svc.Entry(ctx, project, from, p); err != nil {
				oldPath = s.svc.PathBefore(ctx, project, from, to, p)
			}
		}
		var meta service.ArtifactMeta
		if e, err := s.svc.Entry(ctx, project, to, p); err == nil {
			meta = s.svc.ArtifactMetaOf(ctx, project, e.ID, path.Base(p))
		}
		data["Frames"] = map[string]any{"width": meta.Width, "height": meta.Height}
		if oldPath != "" && from != "" {
			data["FromURL"] = s.content.URL(project, from, oldPath)
		}
		data["ToURL"] = s.content.URL(project, to, p)
	}
	s.render(w, r, http.StatusOK, "compare", pageData{Title: "Compare", Wide: true, Data: data})
}

type diffLine struct {
	Kind string // "add", "del", "hunk", "file", ""
	Text string
}

func diffLines(d string) []diffLine {
	if d == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(d, "\n"), "\n")
	out := make([]diffLine, 0, len(lines))
	for _, l := range lines {
		k := ""
		switch {
		case strings.HasPrefix(l, "diff --git"), strings.HasPrefix(l, "index "), strings.HasPrefix(l, "+++ "), strings.HasPrefix(l, "--- "),
			strings.HasPrefix(l, "rename "), strings.HasPrefix(l, "similarity "), strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"):
			k = "file"
		case strings.HasPrefix(l, "@@"):
			k = "hunk"
		case strings.HasPrefix(l, "+"):
			k = "add"
		case strings.HasPrefix(l, "-"):
			k = "del"
		}
		out = append(out, diffLine{Kind: k, Text: l})
	}
	return out
}

func (s *Server) restorePost(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	p := r.FormValue("path")
	res, err := s.svc.Restore(r.Context(), a, project, r.FormValue("version"), p, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dest := "/p/" + url.PathEscape(project) + "/-/history"
	if p != "" {
		dest += "?path=" + url.QueryEscape(p)
	}
	if res.Unchanged {
		dest += sep(dest) + "notice=unchanged"
	} else {
		dest += sep(dest) + "notice=restored"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

func (s *Server) deletePost(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	if r.FormValue("confirm") != project {
		s.fail(w, r, &service.Error{Kind: service.KindInvalid, Msg: "Type the project id to confirm deleting it."})
		return
	}
	if err := s.svc.DeleteProject(r.Context(), a, project); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) membersPage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	pc, err := s.projectContext(r, a, project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ms, err := s.svc.ListMembers(r.Context(), a, project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "members", pageData{Title: "Members", Error: r.URL.Query().Get("error"), Data: map[string]any{
		"P": pc, "Members": ms, "Me": a.Account.Email,
	}})
}

func (s *Server) membersPost(w http.ResponseWriter, r *http.Request, a service.Actor) {
	project := r.PathValue("project")
	var err error
	email := r.FormValue("email")
	switch r.FormValue("action") {
	case "set":
		_, err = s.svc.SetMember(r.Context(), a, project, email, r.FormValue("role"))
	case "remove":
		err = s.svc.RemoveMember(r.Context(), a, project, email)
		if err == nil && strings.EqualFold(email, a.Account.Email) && !a.Account.IsAdmin() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	default:
		err = &service.Error{Kind: service.KindInvalid, Msg: "unknown action"}
	}
	dest := "/p/" + url.PathEscape(project) + "/-/members"
	if err != nil {
		status, msg := s.statusOf(err)
		if status >= 500 {
			s.fail(w, r, err)
			return
		}
		dest += "?error=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	s.renderTokens(w, r, a, nil, "")
}

func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, a service.Actor, created *service.NewToken, errMsg string) {
	ctx := r.Context()
	mine, err := s.svc.ListTokens(ctx, a, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := map[string]any{"Tokens": mine, "Created": created, "MCP": s.svc.Config().BaseURL + "/mcp", "Git": s.svc.Config().BaseURL + "/git/"}
	if a.Account.IsAdmin() {
		all, _ := s.svc.ListTokens(ctx, a, true)
		accts, _ := s.svc.ListAccounts(ctx, a)
		emails := map[int64]string{}
		for _, ac := range accts {
			emails[ac.ID] = ac.Email
		}
		var others []map[string]any
		for _, t := range all {
			if t.AccountID != a.Account.ID {
				others = append(others, map[string]any{"T": t, "Email": emails[t.AccountID]})
			}
		}
		data["Others"] = others
	}
	s.render(w, r, http.StatusOK, "tokens", pageData{Title: "Tokens", Error: errMsg, Data: data})
}

func (s *Server) tokensPost(w http.ResponseWriter, r *http.Request, a service.Actor) {
	ctx := r.Context()
	switch r.FormValue("action") {
	case "create":
		nt, err := s.svc.CreateToken(ctx, a.Account, r.FormValue("label"))
		if err != nil {
			_, msg := s.statusOf(err)
			s.renderTokens(w, r, a, nil, msg)
			return
		}
		// Shown once, in this response; never stored in plain text.
		s.renderTokens(w, r, a, &nt, "")
		return
	case "revoke":
		id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
		if err := s.svc.RevokeToken(ctx, a, id); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

func (s *Server) accountsPage(w http.ResponseWriter, r *http.Request, a service.Actor) {
	accts, err := s.svc.ListAccounts(r.Context(), a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var pending, active, disabled []store.Account
	for _, ac := range accts {
		switch ac.Status {
		case store.StatusPending:
			pending = append(pending, ac)
		case store.StatusActive:
			active = append(active, ac)
		default:
			disabled = append(disabled, ac)
		}
	}
	s.render(w, r, http.StatusOK, "accounts", pageData{Title: "Accounts", Error: r.URL.Query().Get("error"), Data: map[string]any{
		"Pending": pending, "Active": active, "Disabled": disabled, "Me": a.Account.ID,
	}})
}

func (s *Server) accountsPost(w http.ResponseWriter, r *http.Request, a service.Actor) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	err := s.svc.ManageAccount(r.Context(), a, id, service.AccountAction(r.FormValue("action")))
	dest := "/admin/accounts"
	if err != nil {
		status, msg := s.statusOf(err)
		if status >= 500 {
			s.fail(w, r, err)
			return
		}
		dest += "?error=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
