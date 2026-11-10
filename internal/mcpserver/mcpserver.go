// Package mcpserver is Artifex's agent interface (design decision 9): an MCP
// server over Streamable HTTP at /mcp, authenticated with bearer tokens.
// Every tool calls the service, which checks the token's project role.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/service"
)

const instructions = `Artifex hosts HTML that you want to show people. People view it in a browser and comment on any spot; you make every change.

Projects
- A project is a private git repository with members. Roles: commenter (view, comment), editor (also publish and restore), owner (also manage members). list_projects shows yours.
- upload_artifact creates the project if it doesn't exist yet; you become its owner. Project ids are lowercase letters, digits and dashes.

Publishing
- An artifact is an HTML file. The browser draws it exactly as written, so write complete documents: <!doctype html>, <title> (shown as its name), and your CSS.
- Artifacts may load files only from their own project: put stylesheets, scripts, fonts and images in the project and link them with relative paths. Links to other hosts (CDNs) are blocked. Use content_base64 for binary files.
- <meta name="artifact" content="width=1440"> shows an artifact at a fixed width (mockups of screens). Without it, it fills the window like a web page.
- Link artifacts to each other with relative links (<a href="../other/page.html">); the viewer opens them in Artifex.
- A folder can have folder.yaml (title, label, order) and the project project.yaml (title, description, open: the artifact to open first). A folder shows its artifacts together as a canvas.
- To change or delete an existing file, pass its blob id as base: from read_file, list_files, or the files list of your last publish. If the file changed since, nothing is written and you get its current content to merge.
- publish writes many files in one version; use it when files belong together. Every publish is a version people can compare and restore.

Comments
- list_threads (state "open") shows what people asked for. get_thread shows the comment, the element it points at (CSS selector and text), the HTML around it, and whether the artifact changed since.
- After changing an artifact for a thread, reply saying what you did and resolve it with the version from your publish.
- Answer people in their language, briefly.`

// Handler returns the /mcp handler.
func Handler(svc *service.Service, log *zap.Logger) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "artifex", Title: "Artifex", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: instructions})
	t := &tools{svc: svc, log: log}
	t.register(s)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// Behind a reverse proxy every request arrives from localhost with
		// the public Host header, which this protection would refuse. It
		// guards servers without authentication against DNS rebinding;
		// every request here needs a bearer token, which a rebinding page
		// can't have.
		DisableLocalhostProtection: true,
		// The SDK's default is 4 MiB, far below one publish
		// (limits.max_publish_bytes): larger publishes got 413 here.
		MaxRequestBodyBytes: svc.Config().Limits.MaxRequestBytes(),
	})
	verify := func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		a, err := svc.Authenticate(ctx, token)
		if err != nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: a.Account.Email, Extra: map[string]any{"actor": a}}, nil
	}
	return auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)
}

type tools struct {
	svc *service.Service
	log *zap.Logger
}

func actorOf(req *mcp.CallToolRequest) (service.Actor, error) {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return service.Actor{}, errors.New("not authenticated")
	}
	a, ok := req.Extra.TokenInfo.Extra["actor"].(service.Actor)
	if !ok {
		return service.Actor{}, errors.New("not authenticated")
	}
	return a, nil
}

// handler adapts a service call to an MCP tool: it resolves the actor and
// turns service errors into tool error results the agent can read.
func handler[In any](t *tools, name string, fn func(ctx context.Context, a service.Actor, in In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		a, err := actorOf(req)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		out, err := fn(ctx, a, in)
		if err != nil {
			return t.fail(name, a, err), nil, nil
		}
		return nil, out, nil
	}
}

func (t *tools) fail(name string, a service.Actor, err error) *mcp.CallToolResult {
	var ce *service.ConflictError
	if errors.As(err, &ce) {
		b, _ := json.MarshalIndent(ce, "", "  ")
		return errorResult(ce.Error() + "\nMerge your change into the current content below and publish again, passing current_blob as base.\n" + string(b))
	}
	if service.KindOf(err) == 0 {
		t.log.Error("tool failed", zap.String("tool", name), zap.String("by", a.Name()), zap.Error(err))
		return errorResult("internal error; the server log has the details")
	}
	return errorResult(err.Error())
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func add[In any](s *mcp.Server, t *tools, name, desc string, readOnly bool, fn func(ctx context.Context, a service.Actor, in In) (any, error)) {
	ann := &mcp.ToolAnnotations{ReadOnlyHint: readOnly}
	if !readOnly {
		f := false
		ann.DestructiveHint = &f
	}
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc, Annotations: ann}, handler(t, name, fn))
}

// Inputs. Optional fields have omitempty.

type projectIn struct {
	Project string `json:"project" jsonschema:"project id"`
}

type createProjectIn struct {
	Project string `json:"project" jsonschema:"new project id: lowercase letters, digits and dashes"`
	Title   string `json:"title,omitempty" jsonschema:"title shown to people; stored in project.yaml"`
}

type uploadIn struct {
	Project       string  `json:"project" jsonschema:"project id; the project is created if it doesn't exist"`
	Path          string  `json:"path" jsonschema:"file path inside the project, e.g. index.html or screens/wallet.html"`
	Content       *string `json:"content,omitempty" jsonschema:"text content (HTML, CSS, JS, JSON, SVG...)"`
	ContentBase64 *string `json:"content_base64,omitempty" jsonschema:"binary content, base64-encoded"`
	Base          string  `json:"base,omitempty" jsonschema:"blob id of the version you changed; required when the file already exists"`
	Message       string  `json:"message,omitempty" jsonschema:"what changed, shown in the history"`
	Title         string  `json:"title,omitempty" jsonschema:"project title, used only when this upload creates the project"`
}

type publishIn struct {
	Project string               `json:"project" jsonschema:"project id"`
	Message string               `json:"message,omitempty" jsonschema:"what changed, shown in the history"`
	Changes []service.FileChange `json:"changes" jsonschema:"files to write, move or delete, all in one version"`
}

type listFilesIn struct {
	Project   string `json:"project" jsonschema:"project id"`
	Path      string `json:"path,omitempty" jsonschema:"folder; empty for the project's root"`
	Version   string `json:"version,omitempty" jsonschema:"version id; empty for the latest"`
	Recursive bool   `json:"recursive,omitempty" jsonschema:"list every file below the folder"`
}

type fileIn struct {
	Project string `json:"project" jsonschema:"project id"`
	Path    string `json:"path" jsonschema:"file path"`
	Version string `json:"version,omitempty" jsonschema:"version id; empty for the latest"`
}

type historyIn struct {
	Project string `json:"project" jsonschema:"project id"`
	Path    string `json:"path,omitempty" jsonschema:"only versions that changed this file"`
	Limit   int    `json:"limit,omitempty" jsonschema:"how many, newest first (default 50)"`
	Skip    int    `json:"skip,omitempty" jsonschema:"skip this many newest versions, for paging"`
}

type diffIn struct {
	Project string `json:"project" jsonschema:"project id"`
	From    string `json:"from,omitempty" jsonschema:"older version; empty for the parent of to"`
	To      string `json:"to,omitempty" jsonschema:"newer version; empty for the latest"`
	Path    string `json:"path,omitempty" jsonschema:"limit the diff to this file or folder"`
}

type restoreIn struct {
	Project string `json:"project" jsonschema:"project id"`
	Version string `json:"version" jsonschema:"the version to bring back"`
	Path    string `json:"path,omitempty" jsonschema:"a file or folder to restore; empty for the whole project"`
	Message string `json:"message,omitempty" jsonschema:"what this restore is for"`
}

type urlIn struct {
	Project string `json:"project" jsonschema:"project id"`
	Path    string `json:"path,omitempty" jsonschema:"artifact or folder; empty for the project"`
	Version string `json:"version,omitempty" jsonschema:"a version, for an address that shows that version"`
}

type memberIn struct {
	Project string `json:"project" jsonschema:"project id"`
	Email   string `json:"email" jsonschema:"the person's email address"`
	Role    string `json:"role" jsonschema:"commenter, editor or owner"`
}

type removeMemberIn struct {
	Project string `json:"project" jsonschema:"project id"`
	Email   string `json:"email" jsonschema:"the person's email address"`
}

type listThreadsIn struct {
	Project string `json:"project,omitempty" jsonschema:"project id; empty for all your projects"`
	Path    string `json:"path,omitempty" jsonschema:"only threads on this artifact"`
	State   string `json:"state,omitempty" jsonschema:"open or resolved; empty for both"`
	Since   string `json:"since,omitempty" jsonschema:"only threads changed at or after this time (RFC 3339)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"at most this many (default 200)"`
}

type threadIn struct {
	ID int64 `json:"id" jsonschema:"thread id"`
}

type createThreadIn struct {
	Project string         `json:"project" jsonschema:"project id"`
	Path    string         `json:"path" jsonschema:"the artifact the thread is about"`
	Version string         `json:"version,omitempty" jsonschema:"the version it is about; empty for the latest"`
	Body    string         `json:"body" jsonschema:"the comment"`
	Anchor  service.Anchor `json:"anchor,omitempty" jsonschema:"where on the artifact; empty for the artifact as a whole"`
}

type replyIn struct {
	ID   int64  `json:"id" jsonschema:"thread id"`
	Body string `json:"body" jsonschema:"the reply"`
}

type resolveIn struct {
	ID      int64  `json:"id" jsonschema:"thread id"`
	Version string `json:"version,omitempty" jsonschema:"the version with the change that answers the thread"`
	Body    string `json:"body,omitempty" jsonschema:"a closing comment"`
}

type reopenIn struct {
	ID   int64  `json:"id" jsonschema:"thread id"`
	Body string `json:"body,omitempty" jsonschema:"why it is reopened"`
}

func (t *tools) register(s *mcp.Server) {
	svc := t.svc
	add(s, t, "list_projects", "List the projects you belong to, with your role in each and their latest version.", true,
		func(ctx context.Context, a service.Actor, _ struct{}) (any, error) {
			ps, err := svc.ListProjects(ctx, a)
			return map[string]any{"projects": ps}, err
		})
	add(s, t, "create_project", "Create an empty project. You become its owner. upload_artifact also creates the project when it doesn't exist.", false,
		func(ctx context.Context, a service.Actor, in createProjectIn) (any, error) {
			return svc.CreateProject(ctx, a, in.Project, in.Title)
		})
	add(s, t, "upload_artifact", "Write one file (an HTML artifact or any file it uses) as a new version, creating the project if it doesn't exist. Returns the version, the file's new blob id (the base for your next change) and the address people open.", false,
		func(ctx context.Context, a service.Actor, in uploadIn) (any, error) {
			return svc.UploadArtifact(ctx, a, in.Project, in.Path, in.Content, in.ContentBase64, in.Base, in.Title, in.Message)
		})
	add(s, t, "publish", "Write, move and delete several files as one version. Each change to an existing file needs its base blob id; if any changed since, nothing is written and the current content comes back. Threads follow moved artifacts.", false,
		func(ctx context.Context, a service.Actor, in publishIn) (any, error) {
			return svc.Publish(ctx, a, in.Project, in.Message, in.Changes)
		})
	add(s, t, "list_files", "List a folder of a project at a version, with each file's blob id and size.", true,
		func(ctx context.Context, a service.Actor, in listFilesIn) (any, error) {
			v, files, err := svc.ListFiles(ctx, a, in.Project, in.Path, in.Version, in.Recursive)
			return map[string]any{"project": in.Project, "version": v, "files": files}, err
		})
	add(s, t, "read_file", "Read one file at a version (default: the latest). Returns its content and its blob id, the base for changing it.", true,
		func(ctx context.Context, a service.Actor, in fileIn) (any, error) {
			return svc.ReadFile(ctx, a, in.Project, in.Path, in.Version)
		})
	add(s, t, "history", "List versions of a project, or the versions that changed one file, newest first.", true,
		func(ctx context.Context, a service.Actor, in historyIn) (any, error) {
			vs, err := svc.History(ctx, a, in.Project, in.Path, in.Limit, in.Skip)
			return map[string]any{"project": in.Project, "versions": vs}, err
		})
	add(s, t, "diff", "Show the source diff between two versions, optionally for one file or folder.", true,
		func(ctx context.Context, a service.Actor, in diffIn) (any, error) {
			from, to, d, err := svc.Diff(ctx, a, in.Project, in.From, in.To, in.Path)
			return map[string]any{"from": from, "to": to, "diff": d}, err
		})
	add(s, t, "restore", "Bring back an earlier version of a file, a folder or the whole project, as a new version. Nothing is lost: the versions in between stay in the history.", false,
		func(ctx context.Context, a service.Actor, in restoreIn) (any, error) {
			return svc.Restore(ctx, a, in.Project, in.Version, in.Path, in.Message)
		})
	add(s, t, "get_url", "Get the address people open to see a project, folder or artifact.", true,
		func(ctx context.Context, a service.Actor, in urlIn) (any, error) {
			if _, err := svc.Require(ctx, a, in.Project, "commenter"); err != nil {
				return nil, err
			}
			p := ""
			if in.Path != "" {
				var err error
				if p, err = service.CleanDir(in.Path); err != nil {
					return nil, err
				}
			}
			return map[string]any{"url": svc.AppURL(in.Project, p, in.Version)}, nil
		})
	add(s, t, "list_members", "List a project's members and invitations, with their roles.", true,
		func(ctx context.Context, a service.Actor, in projectIn) (any, error) {
			ms, err := svc.ListMembers(ctx, a, in.Project)
			return map[string]any{"project": in.Project, "members": ms}, err
		})
	add(s, t, "set_member", "Give someone a role in a project (owners only). Someone without an account yet is invited and gets the role when their account is verified.", false,
		func(ctx context.Context, a service.Actor, in memberIn) (any, error) {
			return svc.SetMember(ctx, a, in.Project, in.Email, in.Role)
		})
	add(s, t, "remove_member", "Remove someone from a project, or cancel an invitation (owners only).", false,
		func(ctx context.Context, a service.Actor, in removeMemberIn) (any, error) {
			if err := svc.RemoveMember(ctx, a, in.Project, in.Email); err != nil {
				return nil, err
			}
			return map[string]any{"removed": in.Email}, nil
		})
	add(s, t, "list_threads", "List comment threads people started, across your projects or in one, newest activity first. Use state \"open\" for what still needs an answer, and since for what changed recently.", true,
		func(ctx context.Context, a service.Actor, in listThreadsIn) (any, error) {
			q := service.ThreadQuery{Project: in.Project, Path: in.Path, State: in.State, Limit: in.Limit}
			if in.Since != "" {
				ts, err := time.Parse(time.RFC3339, strings.TrimSpace(in.Since))
				if err != nil {
					return nil, &service.Error{Kind: service.KindInvalid, Msg: "since must be an RFC 3339 time, such as 2026-11-10T09:00:00Z"}
				}
				q.Since = ts
			}
			ts, err := svc.ListThreads(ctx, a, q)
			return map[string]any{"threads": ts}, err
		})
	add(s, t, "get_thread", "Read one thread: its comments, the element it points at (selector, text and point), the artifact's HTML around that element at the thread's version, and whether the artifact changed since.", true,
		func(ctx context.Context, a service.Actor, in threadIn) (any, error) {
			return svc.GetThread(ctx, a, in.ID)
		})
	add(s, t, "create_thread", "Start a thread on an artifact, for example to ask people a question about it.", false,
		func(ctx context.Context, a service.Actor, in createThreadIn) (any, error) {
			return svc.CreateThread(ctx, a, in.Project, service.ThreadInput{Path: in.Path, Version: in.Version, Anchor: in.Anchor, Body: in.Body})
		})
	add(s, t, "reply", "Reply in a thread.", false,
		func(ctx context.Context, a service.Actor, in replyIn) (any, error) {
			return svc.Reply(ctx, a, in.ID, in.Body)
		})
	add(s, t, "resolve_thread", "Resolve a thread, naming the version with your change so people can compare before and after.", false,
		func(ctx context.Context, a service.Actor, in resolveIn) (any, error) {
			return svc.Resolve(ctx, a, in.ID, in.Version, in.Body)
		})
	add(s, t, "reopen_thread", "Reopen a resolved thread.", false,
		func(ctx context.Context, a service.Actor, in reopenIn) (any, error) {
			return svc.Reopen(ctx, a, in.ID, in.Body)
		})
}
