//go:build darwin

package applevf

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/richard-sen27/mycelium/internal/sandbox"
)

// Fixed addresses of Lima's user-mode network (the same for vz and qemu).
const (
	hostGatewayIP = "192.168.5.2" // the host as seen from the guest
	guestDNSIP    = "192.168.5.3" // Lima's host resolver
)

// The guest user every sandbox command runs as. A fixed name and uid keep the
// generated scripts independent of the host account.
const (
	guestUser = "mycel"
	guestUID  = 2000
	guestHome = "/home/mycel"
)

// Guest paths of the helpers installed by provisioning.
const (
	guestHelper    = "/usr/local/libexec/mycel/helper"
	guestConfigure = "/usr/local/libexec/mycel/configure"
	guestShell     = "/usr/local/libexec/mycel/shell" // bash --noprofile --norc
	guestWorkspace = "/workspace"
)

// Defaults when a limit is zero (match sandbox.ResolveLimits' defaults).
const (
	defaultCPUs     = 2
	defaultMemory   = 4 << 30
	defaultDisk     = 20 << 30
	defaultMaxProcs = 512
	minMemory       = 512 << 20
	minDisk         = 4 << 30
)

// brokerDockerNested is the profile broker that asks for a private dockerd.
const brokerDockerNested = "docker-nested"

var (
	//go:embed guest/provision.sh.tmpl
	provisionTmplText string
	//go:embed guest/helper.sh
	helperScript string
	//go:embed guest/configure.sh
	configureScript string

	provisionTmpl = template.Must(template.New("provision").Option("missingkey=error").Parse(provisionTmplText))
)

// limaConfig is the subset of Lima's instance YAML this backend writes.
type limaConfig struct {
	VMType            string            `yaml:"vmType"`
	OS                string            `yaml:"os"`
	Arch              string            `yaml:"arch"`
	Images            []limaImageYAML   `yaml:"images"`
	CPUs              int               `yaml:"cpus"`
	Memory            string            `yaml:"memory"`
	Disk              string            `yaml:"disk"`
	Plain             bool              `yaml:"plain"`
	PropagateProxyEnv bool              `yaml:"propagateProxyEnv"`
	Mounts            []struct{}        `yaml:"mounts"`
	Networks          []struct{}        `yaml:"networks"`
	PortForwards      []limaPortForward `yaml:"portForwards"`
	Containerd        limaContainerd    `yaml:"containerd"`
	SSH               limaSSH           `yaml:"ssh"`
	User              limaUser          `yaml:"user"`
	Provision         []limaProvision   `yaml:"provision"`
}

type limaImageYAML struct {
	Location string `yaml:"location"`
	Arch     string `yaml:"arch"`
	Digest   string `yaml:"digest"`
}

type limaPortForward struct {
	GuestIP        string `yaml:"guestIP"`
	GuestPortRange [2]int `yaml:"guestPortRange,flow"`
	Proto          string `yaml:"proto"`
	Ignore         bool   `yaml:"ignore"`
}

type limaContainerd struct {
	System bool `yaml:"system"`
	User   bool `yaml:"user"`
}

type limaSSH struct {
	LoadDotSSHPubKeys bool `yaml:"loadDotSSHPubKeys"`
	ForwardAgent      bool `yaml:"forwardAgent"`
	ForwardX11        bool `yaml:"forwardX11"`
}

type limaUser struct {
	Name  string `yaml:"name"`
	UID   int    `yaml:"uid"`
	Home  string `yaml:"home"`
	Shell string `yaml:"shell"`
}

type limaProvision struct {
	Mode   string `yaml:"mode"`
	Script string `yaml:"script"`
}

// vmParams are the inputs of the generated YAML.
type vmParams struct {
	Image     limaImage
	Limits    sandbox.Limits
	ProxyPort int  // 0: no proxy, nothing may leave the guest
	Docker    bool // install rootless Docker during provisioning
}

// provisionParams fill guest/provision.sh.tmpl.
type provisionParams struct {
	User         string
	UID          int
	ProxyPort    int
	MaxProcs     int
	Docker       bool
	HelperB64    string
	ConfigureB64 string
}

// renderLimaYAML builds the instance YAML: vz, no host mounts, no port
// forwards, no containerd, user-mode networking only, the pinned image, the
// limits, and the provisioning script that installs the firewall.
func renderLimaYAML(p vmParams) ([]byte, error) {
	cpus := p.Limits.CPUs
	if cpus <= 0 {
		cpus = defaultCPUs
	}
	mem := p.Limits.MemoryBytes
	if mem <= 0 {
		mem = defaultMemory
	}
	mem = max(mem, minMemory)
	disk := p.Limits.DiskBytes
	if disk <= 0 {
		disk = defaultDisk
	}
	disk = max(disk, minDisk)
	procs := p.Limits.MaxProcesses
	if procs <= 0 {
		procs = defaultMaxProcs
	}
	if p.ProxyPort < 0 || p.ProxyPort > 65535 {
		return nil, fmt.Errorf("applevf: proxy port %d out of range", p.ProxyPort)
	}

	var script bytes.Buffer
	err := provisionTmpl.Execute(&script, provisionParams{
		User:         guestUser,
		UID:          guestUID,
		ProxyPort:    p.ProxyPort,
		Docker:       p.Docker,
		MaxProcs:     procs,
		HelperB64:    wrapB64(helperScript),
		ConfigureB64: wrapB64(configureScript),
	})
	if err != nil {
		return nil, fmt.Errorf("applevf: render provisioning: %w", err)
	}

	cfg := limaConfig{
		VMType: "vz",
		OS:     "Linux",
		Arch:   p.Image.Arch,
		Images: []limaImageYAML{{Location: p.Image.Location, Arch: p.Image.Arch, Digest: p.Image.Digest}},
		CPUs:   cpus,
		Memory: mib(mem),
		Disk:   mib(disk),
		// Plain mode turns off mounts, port forwarding and containerd as a
		// whole; the explicit empty lists and the ignore-all forward rules
		// below keep that true even if a Lima version changes plain mode.
		Plain:             true,
		PropagateProxyEnv: false,
		Mounts:            []struct{}{},
		Networks:          []struct{}{},
		PortForwards: []limaPortForward{
			{GuestIP: "0.0.0.0", GuestPortRange: [2]int{1, 65535}, Proto: "any", Ignore: true},
			{GuestIP: "127.0.0.1", GuestPortRange: [2]int{1, 65535}, Proto: "any", Ignore: true},
		},
		Containerd: limaContainerd{System: false, User: false},
		SSH:        limaSSH{},
		User:       limaUser{Name: guestUser, UID: guestUID, Home: guestHome, Shell: "/bin/bash"},
		Provision:  []limaProvision{{Mode: "system", Script: script.String()}},
	}
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return nil, fmt.Errorf("applevf: marshal lima yaml: %w", err)
	}
	return append([]byte("# Generated by mycelium (internal/sandbox/applevf). Do not edit.\n"), out...), nil
}

// writeLimaYAML writes the YAML to <dir>/lima.yaml with mode 0600. It never
// follows a symlink: an existing entry is removed and the file is created
// exclusively with O_NOFOLLOW.
func writeLimaYAML(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("applevf: create sandbox dir: %w", err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("applevf: sandbox dir %s is not a directory", dir)
	}
	path := filepath.Join(dir, "lima.yaml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("applevf: write lima yaml: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", fmt.Errorf("applevf: write lima yaml: %w", err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("applevf: write lima yaml: %w", err)
	}
	return path, nil
}

// mib renders a byte count as Lima's size string, rounded up to MiB.
func mib(b int64) string {
	return strconv.FormatInt((b+(1<<20)-1)>>20, 10) + "MiB"
}

// wrapB64 base64-encodes s in 76-column lines for a heredoc.
func wrapB64(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b bytes.Buffer
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteByte('\n')
		enc = enc[76:]
	}
	b.WriteString(enc)
	return b.String()
}

// guestProxyURL rewrites the node proxy URL so the guest reaches it: the host
// becomes Lima's host gateway, port and credentials are kept. It returns the
// rewritten URL and its port; an empty input yields "", 0.
func guestProxyURL(raw string) (string, int, error) {
	if raw == "" {
		return "", 0, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("applevf: proxy url: %w", err)
	}
	if u.Scheme != "http" {
		return "", 0, fmt.Errorf("applevf: proxy url: scheme %q, want http", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", 0, fmt.Errorf("applevf: proxy url has no host")
	}
	port := 80
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return "", 0, fmt.Errorf("applevf: proxy url: bad port %q", p)
		}
		port = n
	}
	u.Host = net.JoinHostPort(hostGatewayIP, strconv.Itoa(port))
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
	return u.String(), port, nil
}

// wantsDockerNested reports whether the profile lists the docker-nested broker.
func wantsDockerNested(brokers []string) bool {
	return slices.Contains(brokers, brokerDockerNested)
}
