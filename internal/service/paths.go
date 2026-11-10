package service

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

var projectIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func validProjectID(id string) bool { return projectIDPattern.MatchString(id) }

// CleanPath checks a file path inside a project and returns it in its one
// accepted form: relative, slash-separated, no "." or ".." segments, never
// inside .git. The first segment can't be "-": the web app's own pages live
// under /p/<project>/-/.
func CleanPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return "", invalid("a file path is required")
	}
	if len(p) > 1024 {
		return "", invalid("path %q is longer than 1024 bytes", p[:40]+"…")
	}
	for _, r := range p {
		if r == '\\' || r == 0 || unicode.IsControl(r) {
			return "", invalid("path %q contains a backslash or control character", p)
		}
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		switch {
		case s == "":
			return "", invalid("path %q has an empty segment", p)
		case s == "." || s == "..":
			return "", invalid("path %q may not contain . or ..", p)
		case strings.EqualFold(s, ".git"):
			return "", invalid("path %q may not touch .git", p)
		case i == 0 && s == "-":
			return "", invalid("a path may not start with the folder \"-\"")
		case len(s) > 255:
			return "", invalid("a segment of %q is longer than 255 bytes", p)
		}
	}
	return p, nil
}

// CleanDir is CleanPath for a folder, where "" (the project's root) is allowed.
func CleanDir(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" || p == "." {
		return "", nil
	}
	return CleanPath(p)
}

// IsHTML reports whether a path is an artifact: an HTML file.
func IsHTML(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".html" || ext == ".htm"
}
