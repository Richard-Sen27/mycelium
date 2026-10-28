package bundle

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ErrPath is a path that would leave /workspace.
var ErrPath = errors.New("bundle: path must be relative and stay inside /workspace")

// Confine maps a path relative to /workspace to its guest path. It refuses
// absolute paths, any ".." element, NUL bytes and the empty path; "." (the
// workspace itself) only when allowRoot is set. The guest resolves symlinks
// inside the sandbox, so a link can only point into the sandbox's own files.
func Confine(p string, allowRoot bool) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return "", fmt.Errorf("%w: %q", ErrPath, p)
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return "", fmt.Errorf("%w: %q", ErrPath, p)
		}
	}
	c := path.Clean(p)
	if c == "." {
		if !allowRoot {
			return "", fmt.Errorf("%w: %q", ErrPath, p)
		}
		return WorkspaceDir, nil
	}
	return WorkspaceDir + "/" + c, nil
}

// ValidatePattern checks a glob pattern relative to /workspace.
func ValidatePattern(p string) error {
	if _, err := Confine(p, false); err != nil {
		return err
	}
	for _, seg := range strings.Split(path.Clean(p), "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("bundle: bad pattern %q: %w", p, err)
		}
	}
	return nil
}

// MatchPattern reports whether name (slash-separated, relative) matches
// pattern; "**" matches any number of path segments, other segments use
// path.Match.
func MatchPattern(pattern, name string) bool {
	return matchSegs(strings.Split(path.Clean(pattern), "/"), strings.Split(name, "/"))
}

func matchSegs(p, n []string) bool {
	for len(p) > 0 {
		if p[0] == "**" {
			for i := 0; i <= len(n); i++ {
				if matchSegs(p[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, _ := path.Match(p[0], n[0]); !ok {
			return false
		}
		p, n = p[1:], n[1:]
	}
	return len(n) == 0
}

// FindFilesArgv lists regular files under /workspace NUL-separated, relative.
func FindFilesArgv() []string {
	return []string{"find", ".", "-type", "f", "-print0"}
}

// FilterGlob turns `find -print0` output into the sorted relative paths that
// match any pattern.
func FilterGlob(out []byte, patterns []string) []string {
	var res []string
	for _, raw := range strings.Split(string(out), "\x00") {
		name := strings.TrimPrefix(raw, "./")
		if name == "" || name == "." {
			continue
		}
		for _, p := range patterns {
			if MatchPattern(p, name) {
				res = append(res, name)
				break
			}
		}
	}
	sort.Strings(res)
	return res
}
