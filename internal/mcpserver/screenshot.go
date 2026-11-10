package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/unalkalkan/artifex/internal/render"
	"github.com/unalkalkan/artifex/internal/service"
)

// screenshot draws an artifact the way Artifex shows it (design decision
// 24): at its own width and height, from its meta tag, unless the agent asks
// for others.
func (t *tools) screenshot(ctx context.Context, a service.Actor, in screenshotIn) (*mcp.CallToolResult, error) {
	svc := t.svc
	if _, err := svc.Require(ctx, a, in.Project, "commenter"); err != nil {
		return nil, err
	}
	p, err := service.CleanPath(in.Path)
	if err != nil {
		return nil, err
	}
	if !service.IsHTML(p) {
		return nil, &service.Error{Kind: service.KindInvalid, Msg: "screenshot draws artifacts: HTML files"}
	}
	commit, err := svc.ResolveVersion(ctx, in.Project, in.Version)
	if err != nil {
		return nil, err
	}
	if commit == "" {
		return nil, &service.Error{Kind: service.KindNotFound, Msg: fmt.Sprintf("project %q has nothing published yet", in.Project)}
	}
	e, err := svc.Entry(ctx, in.Project, commit, p)
	if err != nil {
		return nil, err
	}
	if e.Type != "blob" {
		return nil, &service.Error{Kind: service.KindInvalid, Msg: p + " is a folder; screenshot one of its artifacts"}
	}
	meta := svc.ArtifactMetaOf(ctx, in.Project, e.ID, path.Base(p))
	w, wFrom := in.Width, "the width you asked for"
	switch {
	case w != 0:
	case meta.Width != 0:
		w, wFrom = meta.Width, "its own width"
	default:
		w, wFrom = render.DefaultWidth, "the width a canvas shows an artifact without one at"
	}
	h := in.Height
	if h == 0 {
		h = meta.Height
	}
	res, err := t.o.Renderer.Render(ctx, render.Request{URL: t.o.ContentURL(in.Project, commit, p), Width: w, Height: h, Y: in.Y})
	if err != nil {
		var inv *render.Invalid
		if errors.As(err, &inv) {
			return nil, &service.Error{Kind: service.KindInvalid, Msg: inv.Msg}
		}
		t.log.Warn("screenshot failed", zap.String("project", in.Project), zap.String("path", p), zap.Error(err))
		return errorResult("the screenshot failed; the server log has the details. Try again in a moment."), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s at version %s, drawn %d px wide (%s)", p, commit[:10], res.Width, wFrom)
	switch {
	case in.Height != 0 || meta.Height != 0:
		from := "its own height"
		if in.Height != 0 {
			from = "the height you asked for"
		}
		fmt.Fprintf(&b, " in a window %d px tall (%s).", res.Height, from)
		if res.ContentHeight > res.Height {
			fmt.Fprintf(&b, " Its content is %d px tall and scrolls inside that window; this picture shows %d–%d.", res.ContentHeight, res.ShotY, res.ShotY+res.ShotHeight)
			if end := res.ShotY + res.ShotHeight; end < res.ContentHeight {
				fmt.Fprintf(&b, " Pass y=%d for more.", end)
			}
		}
	default:
		fmt.Fprintf(&b, ". It has no height of its own, so its frame follows its content: %d px.", res.Height)
		if res.ShotHeight < res.Height {
			fmt.Fprintf(&b, " This picture shows %d–%d.", res.ShotY, res.ShotY+res.ShotHeight)
			if end := res.ShotY + res.ShotHeight; end < res.Height {
				fmt.Fprintf(&b, " Pass y=%d for the rest.", end)
			}
		}
	}
	if res.Locked {
		fmt.Fprintf(&b, "\n\nThe page sizes itself to its window (100vh) and grew with its frame, so Artifex stops the frame at %d px: it may look cut off or stretched. Give the artifact its height: <meta name=\"artifact\" content=\"width=%d, height=900\">.", render.LockHeight, res.Width)
	}
	problems := false
	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		problems = true
		b.WriteString("\n\n" + title)
		for _, it := range items {
			b.WriteString("\n- " + it)
		}
	}
	list("Blocked: on other hosts, which artifacts can't load from. Copy them into the project and use relative paths:", res.Blocked)
	list("Missing from the project:", res.Missing)
	list("Errors:", res.Errors)
	if !problems {
		b.WriteString("\n\nNothing was blocked or missing, and no script failed.")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: res.PNG, MIMEType: "image/png"},
		&mcp.TextContent{Text: b.String()},
	}}, nil
}
