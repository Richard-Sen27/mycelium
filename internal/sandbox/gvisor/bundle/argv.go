package bundle

import (
	"errors"
	"fmt"
	"strconv"
)

// Host wraps argv for the machine the sandboxes live on.
type Host interface {
	// Wrap returns the argv that runs argv on the sandbox host.
	Wrap(argv []string) []string
	// Check refuses argv that cannot cross to the sandbox host safely.
	Check(argv []string) error
}

// LocalHost runs commands directly (Linux).
type LocalHost struct{}

// Wrap implements Host.
func (LocalHost) Wrap(argv []string) []string { return argv }

// Check implements Host: NUL bytes cannot be passed to exec.
func (LocalHost) Check(argv []string) error {
	for _, a := range argv {
		for i := 0; i < len(a); i++ {
			if a[i] == 0 {
				return fmt.Errorf("bundle: NUL byte in argument %q", a)
			}
		}
	}
	return nil
}

// WSLHost runs commands as root inside a WSL distro with --exec (no shell).
type WSLHost struct {
	Exe    string // "wsl.exe" when empty
	Distro string // "mycel-sbx" when empty
}

// DefaultDistro is the dedicated sandbox distro on Windows.
const DefaultDistro = "mycel-sbx"

func (h WSLHost) exe() string {
	if h.Exe == "" {
		return "wsl.exe"
	}
	return h.Exe
}

func (h WSLHost) distro() string {
	if h.Distro == "" {
		return DefaultDistro
	}
	return h.Distro
}

// Wrap implements Host.
func (h WSLHost) Wrap(argv []string) []string {
	return append([]string{h.exe(), "-d", h.distro(), "-u", "root", "--exec"}, argv...)
}

// ErrWSLArg is an argument wsl.exe could re-parse: it crosses the Windows
// command line, whose quoting wsl.exe does not promise to undo exactly.
var ErrWSLArg = errors.New("bundle: control character or double quote in an argument for wsl.exe")

// Check implements Host: control characters and double quotes are refused.
func (WSLHost) Check(argv []string) error {
	for _, a := range argv {
		for _, r := range a {
			if r < 0x20 || r == 0x7f || r == '"' {
				return fmt.Errorf("%w: %q", ErrWSLArg, a)
			}
		}
	}
	return nil
}

// WSLStatusArgv checks that WSL works.
func (h WSLHost) WSLStatusArgv() []string { return []string{h.exe(), "--status"} }

// WSLListArgv lists installed distros, one per line (UTF-16 output).
func (h WSLHost) WSLListArgv() []string { return []string{h.exe(), "--list", "--quiet"} }

// DefaultRouteArgv prints the distro's default route, whose gateway is the
// Windows host's address on the WSL interface.
func (h WSLHost) DefaultRouteArgv() []string {
	return h.Wrap([]string{"ip", "route", "show", "default"})
}

// DetachScript is the one fixed shell script used: on WSL, `runsc run
// --detach` runs through setsid with stdio on /dev/null so the sandbox does
// not hold wsl.exe's relay pipes and wsl.exe returns. It is fed on stdin to
// `sh -s` and takes everything as positional parameters (log file, then the
// runsc argv); nothing is interpolated into it.
const DetachScript = "log=$1; shift; exec setsid -w \"$@\" </dev/null >/dev/null 2>>\"$log\"\n"

// DetachArgv runs argv through DetachScript, appending stderr to logFile.
func DetachArgv(logFile string, argv []string) []string {
	return append([]string{"sh", "-s", "--", logFile}, argv...)
}

// Runsc builds runsc argv. Global flags precede the subcommand.
type Runsc struct {
	Bin     string // "runsc" when empty
	Root    string // --root state directory
	Network string // "sandbox" or "none"
	// Overlay is the medium of the root overlay: "dir=<path>" puts the
	// overlay's backing file in the sandbox directory; "self" when empty.
	Overlay string
}

func (r Runsc) bin() string {
	if r.Bin == "" {
		return "runsc"
	}
	return r.Bin
}

func (r Runsc) base() []string {
	network := r.Network
	if network == "" {
		network = "none"
	}
	overlay := r.Overlay
	if overlay == "" {
		overlay = "self"
	}
	return []string{r.bin(),
		"--root=" + r.Root,
		"--network=" + network,
		"--host-uds=none",
		"--host-fifo=none",
		"--overlay2=root:" + overlay,
	}
}

func (r Runsc) cmd(args ...string) []string { return append(r.base(), args...) }

// Version is `runsc --version` (no global flags).
func (r Runsc) Version() []string { return []string{r.bin(), "--version"} }

// Run starts the container detached from the bundle directory.
func (r Runsc) Run(bundleDir, id string) []string {
	return r.cmd("run", "--detach", "--bundle="+bundleDir, id)
}

// ExecProcess runs the process described by a process.json file and writes
// the sandbox-internal pid to pidFile.
func (r Runsc) ExecProcess(id, processFile, pidFile string) []string {
	return r.cmd("exec", "--process="+processFile, "--internal-pid-file="+pidFile, id)
}

// ExecAs runs a helper argv in /workspace as user ("uid:gid").
func (r Runsc) ExecAs(id, user string, argv ...string) []string {
	return append(r.cmd("exec", "--cwd="+WorkspaceDir, "--user="+user, id), argv...)
}

// Pause freezes the sandbox.
func (r Runsc) Pause(id string) []string { return r.cmd("pause", id) }

// Resume thaws the sandbox.
func (r Runsc) Resume(id string) []string { return r.cmd("resume", id) }

// Stats prints one stats event as JSON.
func (r Runsc) Stats(id string) []string { return r.cmd("events", "--stats", id) }

// PS lists the sandbox's processes as JSON.
func (r Runsc) PS(id string) []string { return r.cmd("ps", "--format=json", id) }

// KillAll signals every process in the sandbox.
func (r Runsc) KillAll(id, sig string) []string { return r.cmd("kill", "--all", id, sig) }

// KillPid signals one process by its sandbox-internal pid.
func (r Runsc) KillPid(id string, pid int, sig string) []string {
	return r.cmd("kill", "--pid="+strconv.Itoa(pid), id, sig)
}

// Delete removes the container even when it is still running.
func (r Runsc) Delete(id string) []string { return r.cmd("delete", "--force", id) }

// State prints the container state; it fails once the container is gone.
func (r Runsc) State(id string) []string { return r.cmd("state", id) }

// List prints the ids of all containers under --root.
func (r Runsc) List() []string { return r.cmd("list", "--quiet") }

// Docker builds docker client argv for image export.
type Docker struct {
	Bin string // "docker" when empty
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

// Version checks the client is present.
func (d Docker) Version() []string { return []string{d.bin(), "--version"} }

// Pull fetches an image.
func (d Docker) Pull(image string) []string { return []string{d.bin(), "pull", "--", image} }

// ImageID prints the image's content digest (sha256:…).
func (d Docker) ImageID(image string) []string {
	return []string{d.bin(), "image", "inspect", "--format={{.Id}}", "--", image}
}

// Create creates a stopped container from the image and prints its id.
func (d Docker) Create(image string) []string {
	return []string{d.bin(), "create", "--pull=never", "--network=none", "--", image, "/nonexistent"}
}

// Export writes the container's filesystem as a tar stream to stdout.
func (d Docker) Export(container string) []string {
	return []string{d.bin(), "export", "--", container}
}

// Remove deletes the export container.
func (d Docker) Remove(container string) []string {
	return []string{d.bin(), "rm", "--force", "--", container}
}

// File-system helpers run on the sandbox host (wrapped by Host).

// MkdirArgv creates directories with mode 0700.
func MkdirArgv(dirs ...string) []string {
	return append([]string{"mkdir", "-p", "-m", "0700", "--"}, dirs...)
}

// MkdirModeArgv creates one directory with mode (octal).
func MkdirModeArgv(mode uint32, dir string) []string {
	return []string{"mkdir", "-p", "-m", strconv.FormatUint(uint64(mode&0o7777), 8), "--", dir}
}

// CreatePrivateArgv creates an empty file with mode 0600 (before secrets
// are written into it).
func CreatePrivateArgv(path string) []string {
	return []string{"install", "-m", "0600", "/dev/null", path}
}

// WriteFileArgv writes stdin to path (keeping the mode of an existing file).
func WriteFileArgv(path string) []string {
	return []string{"dd", "of=" + path, "bs=1M", "status=none"}
}

// ChmodArgv sets a file mode (octal).
func ChmodArgv(mode uint32, path string) []string {
	return []string{"chmod", strconv.FormatUint(uint64(mode&0o7777), 8), "--", path}
}

// ChownArgv sets numeric owner and group.
func ChownArgv(u User, path string) []string {
	return []string{"chown", fmt.Sprintf("%d:%d", u.UID, u.GID), "--", path}
}

// MoveArgv renames a file or directory.
func MoveArgv(from, to string) []string { return []string{"mv", "-f", "-T", "--", from, to} }

// RemoveAllArgv removes a tree without crossing file systems.
func RemoveAllArgv(path string) []string {
	return []string{"rm", "-rf", "--one-file-system", "--", path}
}

// RemoveArgv removes one file.
func RemoveArgv(path string) []string { return []string{"rm", "-f", "--", path} }

// TestDirArgv exits 0 when path is a directory.
func TestDirArgv(path string) []string { return []string{"test", "-d", path} }

// CatArgv prints a file.
func CatArgv(path string) []string { return []string{"cat", "--", path} }

// ExtractArgv unpacks a (filtered) tar stream from stdin into dir, keeping
// numeric owners.
func ExtractArgv(dir string) []string {
	return []string{"tar", "-x", "--numeric-owner", "-C", dir, "-f", "-"}
}

// DiskUsageArgv prints the bytes used under dir (`du -s -B1`).
func DiskUsageArgv(dir string) []string {
	return []string{"du", "-s", "-B1", "-x", "--", dir}
}

// ToolVersionArgv checks a host tool exists.
func ToolVersionArgv(tool string) []string {
	switch tool {
	case "ip":
		return []string{"ip", "-V"}
	default:
		return []string{tool, "--version"}
	}
}

// Listing commands used by garbage collection.
var (
	NetnsListArgv   = []string{"ip", "netns", "list"}
	LinkListArgv    = []string{"ip", "-o", "link", "show"}
	AddrListArgv    = []string{"ip", "-o", "-4", "addr", "show"}
	NFTTablesArgv   = []string{"nft", "list", "tables"}
	IPTablesArgv    = []string{"iptables", "-w", "-S"}
	IPTablesNatArgv = []string{"iptables", "-w", "-t", "nat", "-S"}
	IP6TablesArgv   = []string{"ip6tables", "-w", "-S"}
)
