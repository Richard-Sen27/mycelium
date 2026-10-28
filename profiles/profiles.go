// Package profiles holds the sandbox profiles shipped with the binary (spec:
// "Sandbox model → Profiles"). Each profile is a YAML file in this directory,
// embedded at build time, whose keys are the field names of the proto
// mycel.v1.Profile. limits.wall_clock is a Go duration string; it is parsed
// on load and stored in canonical form ("1h0m0s").
//
// The fake profile (backends: [fake]) is CI-only; callers decide whether to
// expose it (see IsFake).
package profiles

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	"github.com/richard-sen27/mycelium/internal/sandbox"
)

//go:embed *.yaml
var files embed.FS

// ErrNotFound is returned by Get for an unknown profile name.
var ErrNotFound = errors.New("profiles: not found")

var (
	knownBackends = map[string]bool{
		sandbox.BackendAppleVF: true, sandbox.BackendWSL2GVisor: true, sandbox.BackendGVisor: true,
		sandbox.BackendFirecracker: true, sandbox.BackendFake: true,
	}
	knownBrokers = map[string]bool{
		"ollama": true, "docker-nested": true, "docker-host": true, "gpu": true,
	}
	knownApprovals = map[string]bool{
		"rw-mount": true, "gpu": true, "egress-outside-allowlist": true,
	}
	nameRE    = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	labelRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	numericRE = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]*)$`)
	imageRE   = regexp.MustCompile(func() string {
		// OCI/distribution reference: [registry[:port]/]repo[:tag][@digest].
		domainComp := `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])`
		domain := domainComp + `(?:\.` + domainComp + `)*(?::[0-9]+)?`
		pathComp := `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
		name := `(?:` + domain + `/)?` + pathComp + `(?:/` + pathComp + `)*`
		tag := `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`
		digest := `[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}`
		return `^` + name + `(?::` + tag + `)?(?:@` + digest + `)?$`
	}())
)

// file is the YAML shape of one profile.
type file struct {
	Name             string      `yaml:"name"`
	Version          int32       `yaml:"version"`
	Description      string      `yaml:"description"`
	Backends         []string    `yaml:"backends"`
	Image            string      `yaml:"image"`
	Limits           *limitsFile `yaml:"limits"`
	Egress           *egressFile `yaml:"egress"`
	Brokers          []string    `yaml:"brokers"`
	Mounts           []mountFile `yaml:"mounts"`
	RequiresApproval []string    `yaml:"requires_approval"`
}

type limitsFile struct {
	CPUs         int32  `yaml:"cpus"`
	MemoryBytes  int64  `yaml:"memory_bytes"`
	DiskBytes    int64  `yaml:"disk_bytes"`
	MaxProcesses int32  `yaml:"max_processes"`
	WallClock    string `yaml:"wall_clock"`
	MaxTokens    int64  `yaml:"max_tokens"`
}

type egressFile struct {
	Allow []string `yaml:"allow"`
}

type mountFile struct {
	HostPath  string `yaml:"host_path"`
	GuestPath string `yaml:"guest_path"`
	ReadOnly  bool   `yaml:"read_only"`
}

var (
	loadOnce sync.Once
	loaded   []*mycelv1.Profile
	errLoad  error
)

// All returns every embedded profile, sorted by name. The returned messages
// are copies the caller may modify.
func All() ([]*mycelv1.Profile, error) {
	loadOnce.Do(func() { loaded, errLoad = loadFS(files) })
	if errLoad != nil {
		return nil, errLoad
	}
	out := make([]*mycelv1.Profile, len(loaded))
	for i, p := range loaded {
		out[i] = proto.Clone(p).(*mycelv1.Profile)
	}
	return out, nil
}

// Get returns the embedded profile with the given name, or an error wrapping
// ErrNotFound.
func Get(name string) (*mycelv1.Profile, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.GetName() == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// IsFake reports whether p is a CI-only profile: its only backend is fake.
func IsFake(p *mycelv1.Profile) bool {
	b := p.GetBackends()
	return len(b) == 1 && b[0] == "fake"
}

// loadFS parses every *.yaml in fsys. A profile's name must match its file
// name and names must be unique.
func loadFS(fsys fs.FS) ([]*mycelv1.Profile, error) {
	names, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	var out []*mycelv1.Profile
	for _, fn := range names {
		data, err := fs.ReadFile(fsys, fn)
		if err != nil {
			return nil, err
		}
		p, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("profiles: %s: %w", fn, err)
		}
		if want := strings.TrimSuffix(fn, ".yaml"); p.GetName() != want {
			return nil, fmt.Errorf("profiles: %s: name %q does not match the file name", fn, p.GetName())
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// Parse decodes and validates one profile document. Unknown keys are errors.
func Parse(data []byte) (*mycelv1.Profile, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode: only one YAML document is allowed")
	}
	p := &mycelv1.Profile{
		Name:             f.Name,
		Version:          f.Version,
		Description:      f.Description,
		Backends:         f.Backends,
		Image:            f.Image,
		Brokers:          f.Brokers,
		RequiresApproval: f.RequiresApproval,
		Limits:           &mycelv1.Limits{},
		Egress:           &mycelv1.EgressPolicy{},
	}
	if f.Limits != nil {
		p.Limits = &mycelv1.Limits{
			Cpus:         f.Limits.CPUs,
			MemoryBytes:  f.Limits.MemoryBytes,
			DiskBytes:    f.Limits.DiskBytes,
			MaxProcesses: f.Limits.MaxProcesses,
			MaxTokens:    f.Limits.MaxTokens,
		}
		if f.Limits.WallClock != "" {
			d, err := time.ParseDuration(f.Limits.WallClock)
			if err != nil {
				return nil, fmt.Errorf("limits.wall_clock: %w", err)
			}
			p.Limits.WallClock = d.String()
		}
	}
	if f.Egress != nil {
		p.Egress.Allow = f.Egress.Allow
	}
	for _, m := range f.Mounts {
		p.Mounts = append(p.Mounts, &mycelv1.Mount{HostPath: m.HostPath, GuestPath: m.GuestPath, ReadOnly: m.ReadOnly})
	}
	if err := Validate(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate checks a profile's static rules: name, version, backends, image,
// limits, egress rules, brokers and the shape of mounts.
func Validate(p *mycelv1.Profile) error {
	if p == nil {
		return errors.New("profile is nil")
	}
	if !nameRE.MatchString(p.GetName()) {
		return fmt.Errorf("name %q: must match [a-z0-9-]{1,32}", p.GetName())
	}
	if p.GetVersion() < 1 {
		return fmt.Errorf("version %d: must be at least 1", p.GetVersion())
	}
	if len(p.GetBackends()) == 0 {
		return errors.New("backends: at least one is required")
	}
	seen := map[string]bool{}
	for _, b := range p.GetBackends() {
		if !knownBackends[b] {
			return fmt.Errorf("backends: unknown backend %q", b)
		}
		if seen[b] {
			return fmt.Errorf("backends: %q listed twice", b)
		}
		seen[b] = true
	}
	if seen[sandbox.BackendFake] && len(p.GetBackends()) != 1 {
		return errors.New("backends: fake must be the only backend when listed")
	}
	if img := p.GetImage(); img == "" {
		return errors.New("image: required")
	} else if len(img) > 512 || !imageRE.MatchString(img) {
		return fmt.Errorf("image %q: not a valid OCI reference ([registry/]repo[:tag][@digest])", img)
	}
	if l := p.GetLimits(); l != nil {
		if l.GetCpus() < 0 || l.GetMemoryBytes() < 0 || l.GetDiskBytes() < 0 || l.GetMaxProcesses() < 0 || l.GetMaxTokens() < 0 {
			return errors.New("limits: values must not be negative")
		}
		if w := l.GetWallClock(); w != "" {
			d, err := time.ParseDuration(w)
			if err != nil {
				return fmt.Errorf("limits.wall_clock: %w", err)
			}
			if d <= 0 {
				return fmt.Errorf("limits.wall_clock %q: must be positive", w)
			}
		}
	}
	for _, r := range p.GetEgress().GetAllow() {
		if err := ValidateEgressRule(r); err != nil {
			return fmt.Errorf("egress.allow: %w", err)
		}
		if err := CheckEgressDestination(r); err != nil {
			return fmt.Errorf("egress.allow: %w", err)
		}
	}
	for _, b := range p.GetBrokers() {
		if !knownBrokers[b] {
			return fmt.Errorf("brokers: unknown broker %q", b)
		}
	}
	for _, a := range p.GetRequiresApproval() {
		if !knownApprovals[a] {
			return fmt.Errorf("requires_approval: unknown value %q", a)
		}
	}
	for _, m := range p.GetMounts() {
		err := sandbox.ValidateMount(sandbox.Mount{HostPath: m.GetHostPath(), GuestPath: m.GetGuestPath(), ReadOnly: m.GetReadOnly()})
		if err != nil {
			return fmt.Errorf("mounts: %w", err)
		}
	}
	return nil
}

// egressRule is a parsed allowlist entry.
type egressRule struct {
	host     string // lower-case host without "*." prefix, or the IP literal
	ip       net.IP // set for IP literals
	wildcard bool
	port     int // 0 = default ports
}

// parseEgressRule implements the proxy's grammar: "host", "*.suffix" (at
// least two labels), IPv4, "[v6]", each optionally followed by ":port";
// hosts are lower-case.
func parseEgressRule(rule string) (egressRule, error) {
	var r egressRule
	if rule == "" {
		return r, errors.New("empty rule")
	}
	parsePort := func(s string) error {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 || strings.HasPrefix(s, "+") {
			return fmt.Errorf("rule %q: invalid port", rule)
		}
		r.port = n
		return nil
	}
	if strings.HasPrefix(rule, "[") {
		end := strings.IndexByte(rule, ']')
		if end < 0 {
			return r, fmt.Errorf("rule %q: unterminated [", rule)
		}
		ip := net.ParseIP(rule[1:end])
		if ip == nil || ip.To4() != nil || strings.Contains(rule[1:end], "%") {
			return r, fmt.Errorf("rule %q: invalid IPv6 literal", rule)
		}
		r.host, r.ip = ip.String(), ip
		if rest := rule[end+1:]; rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return r, fmt.Errorf("rule %q: junk after ]", rule)
			}
			if err := parsePort(rest[1:]); err != nil {
				return r, err
			}
		}
		return r, nil
	}
	host := rule
	if i := strings.LastIndexByte(rule, ':'); i >= 0 {
		host = rule[:i]
		if strings.Contains(host, ":") {
			return r, fmt.Errorf("rule %q: IPv6 literals must be written in brackets", rule)
		}
		if err := parsePort(rule[i+1:]); err != nil {
			return r, err
		}
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		r.host, r.ip = host, ip
		return r, nil
	}
	r.wildcard = strings.HasPrefix(host, "*.")
	r.host = strings.TrimPrefix(host, "*.")
	if r.host == "" || len(r.host) > 253 {
		return r, fmt.Errorf("rule %q: invalid host", rule)
	}
	labels := strings.Split(r.host, ".")
	for _, l := range labels {
		if !labelRE.MatchString(l) {
			return r, fmt.Errorf("rule %q: invalid host label %q (lower-case letters, digits, -)", rule, l)
		}
	}
	if r.wildcard && len(labels) < 2 {
		return r, fmt.Errorf("rule %q: a wildcard needs a suffix of at least two labels", rule)
	}
	return r, nil
}

// ValidateEgressRule checks the syntax of one allowlist entry, using the
// egress proxy's grammar: "host", "*.suffix" with at least two labels,
// IPv4, "[v6]", each optionally followed by ":port"; hosts are lower-case.
func ValidateEgressRule(rule string) error {
	_, err := parseEgressRule(rule)
	return err
}

// CheckEgressDestination refuses syntactically valid rules that point at
// the node itself or at addresses that are never legitimate destinations:
// loopback, link-local, unspecified and multicast IPs, "localhost", and
// names whose last label is numeric or hex (127.1, 0x7f000001, *.0.1).
func CheckEgressDestination(rule string) error {
	r, err := parseEgressRule(rule)
	if err != nil {
		return err
	}
	if r.ip != nil {
		ip := r.ip
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("rule %q: loopback, link-local, unspecified and multicast addresses are refused", rule)
		}
		return nil
	}
	if r.host == "localhost" || strings.HasSuffix(r.host, ".localhost") {
		return fmt.Errorf("rule %q: localhost is refused", rule)
	}
	labels := strings.Split(r.host, ".")
	if numericRE.MatchString(labels[len(labels)-1]) {
		return fmt.Errorf("rule %q: numeric host names are refused", rule)
	}
	return nil
}
