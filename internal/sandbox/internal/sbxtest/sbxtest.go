// Package sbxtest turns a test binary into a portable helper command for
// sandbox tests, so they need no shell. A test's TestMain calls MaybeRun
// first; a sandboxed command is then [Exe(), mode, args...] with
// Env {EnvKey: "1"}.
//
// Modes:
//
//	env              print the environment, one sorted entry per line
//	echo OUT ERR     print OUT to stdout and ERR to stderr
//	pwd              print the working directory
//	exit N           exit with code N
//	sleep            sleep for a minute (dies on TERM)
//	ignoreterm       ignore TERM, print "ready", sleep for a minute
//	write P DATA...  write DATA to file P (relative to cwd), creating dirs
//	flood N          write N bytes of 'x' to stdout
//	writesleep P D   write D to file P, then sleep for a minute
//	spawn MODE THEN  start "MODE" as a background child, print "child <pid>",
//	                 then exit (THEN=exit) or sleep (THEN=sleep)
package sbxtest

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// EnvKey marks a process as a helper invocation.
const EnvKey = "SBXTEST_HELPER"

// Exe returns the absolute path of the running test binary.
func Exe() string {
	p, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return p
}

// Env is the environment a helper command needs. GORACE drops the race
// runtime's one-second sleep at exit when tests run with -race.
func Env() map[string]string {
	return map[string]string{EnvKey: "1", "GORACE": "atexit_sleep_ms=0"}
}

// ChildPID parses the "child <pid>" line printed by mode spawn.
func ChildPID(out string) int {
	for _, line := range strings.Split(out, "\n") {
		if pid, ok := strings.CutPrefix(line, "child "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(pid))
			return n
		}
	}
	return 0
}

// Argv builds a helper command line.
func Argv(mode string, args ...string) []string {
	return append([]string{Exe(), mode}, args...)
}

// MaybeRun runs the helper and exits when the process is a helper.
func MaybeRun() {
	if os.Getenv(EnvKey) != "1" || len(os.Args) < 2 {
		return
	}
	os.Exit(run(os.Args[1], os.Args[2:]))
}

func run(mode string, args []string) int {
	switch mode {
	case "env":
		env := os.Environ()
		sort.Strings(env)
		fmt.Println(strings.Join(env, "\n"))
	case "echo":
		_, _ = os.Stdout.WriteString(args[0])
		_, _ = os.Stderr.WriteString(args[1])
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
	case "exit":
		n, _ := strconv.Atoi(args[0])
		return n
	case "sleep":
		time.Sleep(time.Minute)
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(time.Minute)
	case "write":
		// The path comes from the test itself; the helper runs in a sandbox
		// workspace.
		p := filepath.FromSlash(args[0])
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil { //nolint:gosec // G703: test-controlled path
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := os.WriteFile(p, []byte(strings.Join(args[1:], " ")), 0o600); err != nil { //nolint:gosec // G703: test-controlled path
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	case "writesleep":
		p := filepath.FromSlash(args[0])
		if err := os.WriteFile(p, []byte(args[1]), 0o600); err != nil { //nolint:gosec // G703: test-controlled path
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		time.Sleep(time.Minute)
	case "spawn":
		// spawn CHILDMODE THEN: start a background child (same process
		// group, same stdout), print "child <pid>", then exit or sleep.
		cmd := exec.Command(Exe(), args[0]) //nolint:gosec // G204: the test binary itself
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Printf("child %d\n", cmd.Process.Pid)
		if args[1] == "sleep" {
			time.Sleep(time.Minute)
		}
	case "flood":
		n, _ := strconv.Atoi(args[0])
		buf := []byte(strings.Repeat("x", 4096))
		for n > 0 {
			k := min(n, len(buf))
			_, _ = os.Stdout.Write(buf[:k])
			n -= k
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode", mode)
		return 99
	}
	return 0
}
