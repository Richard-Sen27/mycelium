package sandbox

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateMount(t *testing.T) {
	tmp := t.TempDir()
	mk := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := mk(filepath.Join(tmp, "data"))
	file := filepath.Join(tmp, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withDocker := mk(filepath.Join(tmp, "run1"))
	if err := os.WriteFile(filepath.Join(withDocker, "docker.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withPodman := mk(filepath.Join(tmp, "run2"))
	if err := os.WriteFile(filepath.Join(withPodman, "podman.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	withContainerd := mk(filepath.Join(tmp, "run3"))
	if err := os.WriteFile(filepath.Join(withContainerd, "containerd.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	type tc struct {
		name      string
		host, gst string
		wantErr   bool
	}
	cases := []tc{
		{"ok", ok, "/data", false},
		{"ok guest opt", ok, "/opt/data", false},
		{"not existing", filepath.Join(tmp, "new"), "/data", true},
		{"a file", file, "/data", true},
		{"dotdot in raw path", ok + string(filepath.Separator) + ".." + string(filepath.Separator) + "data", "/data", true},
		{"dir with docker.sock", withDocker, "/data", true},
		{"dir with podman.sock", withPodman, "/data", true},
		{"dir with containerd.sock", withContainerd, "/data", true},
		{"empty host", "", "/data", true},
		{"relative host", "data", "/data", true},
		{"guest empty", ok, "", true},
		{"guest relative", ok, "data", true},
		{"guest root", ok, "/", true},
		{"guest workspace", ok, "/workspace", true},
		{"guest under workspace", ok, "/workspace/in", true},
		{"guest cleaned into workspace", ok, "/x/../workspace/in", true},
	}
	for _, g := range []string{"/proc", "/sys/fs", "/dev", "/etc/x", "/usr/local", "/bin", "/sbin", "/lib", "/lib64", "/libx32/a", "/run/secrets", "/boot"} {
		cases = append(cases, tc{"guest " + g, ok, g, true})
	}
	if home, err := os.UserHomeDir(); err == nil {
		cases = append(cases,
			tc{"home", home, "/data", true},
			tc{"home parent", filepath.Dir(home), "/data", true},
		)
		for _, s := range []string{".ssh", ".aws", ".gnupg", ".kube", ".docker", ".colima", ".orbstack", ".lima",
			".config/gcloud", ".config", "Library/Keychains", "Library", ".ssh/keys"} {
			cases = append(cases, tc{"home " + s, filepath.Join(home, filepath.FromSlash(s)), "/data", true})
		}
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{
			"/", "/etc", "/etc/ssh", "/usr/local", "/var", "/bin", "/sbin", "/System", "/Library", "/private",
			"/tmp", "/private/tmp", "/var/tmp", "/private/var", "/Volumes", "/mnt", "/media", "/var/root",
			"/var/lib/docker", "/private/var/db", "/Library/Keychains", "/var/run/docker.sock", "/run/podman.sock",
			"/proc/1", "/sys", "/dev", "/boot", "/root",
		} {
			cases = append(cases, tc{"system " + p, p, "/data", true})
		}
		link := func(name, target string) string {
			p := filepath.Join(tmp, name)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			return p
		}
		evil := link("evil", "/etc")
		chain := link("chain1", link("chain2", "/etc"))
		cases = append(cases,
			tc{"symlink to etc", evil, "/data", true},
			tc{"below symlink to etc", filepath.Join(evil, "ssh"), "/data", true},
			tc{"symlink chain to etc", chain, "/data", true},
			tc{"dangling symlink", link("dangling", filepath.Join(tmp, "nowhere")), "/data", true},
			tc{"symlink to ok", link("good", ok), "/data", false},
			tc{"dotdot to etc", ok + "/../../../../../../../../etc", "/data", true},
		)
		if home, err := os.UserHomeDir(); err == nil {
			cases = append(cases, tc{"symlink to home", link("home", home), "/data", true})
		}
		// A path that is itself a socket.
		if d, err := os.MkdirTemp("", "m"); err == nil {
			defer func() { _ = os.RemoveAll(d) }()
			sock := filepath.Join(d, "x.sock")
			if l, err := net.Listen("unix", sock); err == nil {
				defer func() { _ = l.Close() }()
				cases = append(cases, tc{"socket", sock, "/data", true})
			}
		}
	} else {
		cases = append(cases,
			tc{"drive root", `C:\`, "/data", true},
			tc{"windows dir", `C:\Windows\System32`, "/data", true},
			tc{"program files", `C:\Program Files`, "/data", true},
			tc{"unc", `\\server\share\x`, "/data", true},
			tc{"device path", `\\.\C:\x`, "/data", true},
			tc{"long path", `\\?\C:\x`, "/data", true},
			tc{"drive relative", `C:x`, "/data", true},
			tc{"slash path", `/data`, "/data", true},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateMount(Mount{HostPath: c.host, GuestPath: c.gst})
			if c.wantErr != (err != nil) {
				t.Fatalf("ValidateMount(%q, %q) = %v", c.host, c.gst, err)
			}
			if err != nil && !errors.Is(err, ErrMount) {
				t.Fatalf("error does not wrap ErrMount: %v", err)
			}
		})
	}
}

func TestResolveMount(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	host := dir
	if runtime.GOOS != "windows" {
		host = filepath.Join(tmp, "link")
		if err := os.Symlink(dir, host); err != nil {
			t.Fatal(err)
		}
	}
	m, err := ResolveMount(Mount{HostPath: host, GuestPath: "/data/./x/", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if m.HostPath != want || m.GuestPath != "/data/x" || !m.ReadOnly {
		t.Fatalf("ResolveMount = %+v, want host %s", m, want)
	}
}

func TestValidateEnv(t *testing.T) {
	if err := ValidateEnv(map[string]string{"A": "1", "_x": "2", "PATH": "/bin", "Z9": ""}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnv(nil); err != nil {
		t.Fatal(err)
	}
	for _, env := range []map[string]string{
		{"": "x"}, {"1A": "x"}, {"A-B": "x"}, {"A B": "x"}, {"A=B": "x"},
		{"MYCEL_TOKEN": "x"}, {"mycel_token": "x"},
		{"HTTP_PROXY": "x"}, {"Http_Proxy": "x"}, {"https_proxy": "x"}, {"no_proxy": "*,"},
		{"NO_PROXY": ""}, {"ALL_PROXY": "x"}, {"all_proxy": "x"}, {"FTP_PROXY": "x"},
		{"A": "x\x00y"},
	} {
		if err := ValidateEnv(env); !errors.Is(err, ErrEnv) {
			t.Errorf("ValidateEnv(%q) = %v", env, err)
		}
	}
}
