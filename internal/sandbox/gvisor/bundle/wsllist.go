package bundle

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// WSLDistroListed interprets `wsl --list --quiet`: stdout is its output
// (UTF-16 or UTF-8) and runErr the command's error. On a machine with WSL
// but no distro at all, wsl.exe prints "Windows Subsystem for Linux has no
// installed distributions" and exits non-zero; that, or a failure with no
// output, means the distro is not installed rather than an error. A command
// that could not run at all (not found, cancelled) stays an error.
func WSLDistroListed(stdout []byte, runErr error, distro string) (bool, error) {
	text := DecodeWSLText(stdout)
	if runErr == nil {
		return hasLine(text, distro), nil
	}
	if errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist) ||
		errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return false, runErr
	}
	stderr := ""
	var ee *ExitError
	if errors.As(runErr, &ee) {
		stderr = DecodeWSLText([]byte(ee.Stderr))
	}
	if WSLNoDistros(text+"\n"+stderr+"\n"+runErr.Error()) ||
		(strings.TrimSpace(strings.Trim(text, "\uFEFF\x00")) == "" && strings.TrimSpace(stderr) == "") {
		return false, nil
	}
	return false, runErr
}

// WSLNoDistros reports whether wsl.exe's text says that no distribution is
// installed (the English message or its error code, which is not localised).
func WSLNoDistros(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "has no installed distributions") ||
		strings.Contains(l, "wsl_e_default_distro_not_found")
}
