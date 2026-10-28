package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Labels the broker puts on every container it creates.
const (
	LabelGrant   = "mycel.grant"
	LabelSandbox = "mycel.sandbox"
)

// Node defaults and hard caps for brokered containers.
const (
	defaultNanoCPUs  = 2e9
	defaultMemory    = 4 << 30
	maxPids          = 1024
	maxContainers    = 8
	maxShmSize       = 256 << 20
	maxTmpfsSize     = 1 << 30
	maxNofile        = 65536
	maxNproc         = 1024
	maxRestartRetry  = 3
	cgroupParentBase = "mycel-"
)

// apiError is a refusal with its HTTP status.
type apiError struct {
	status int
	msg    string // shown to the client
	reason string // audited instead of msg when set
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

// createInput is what rewriteCreate needs from the grant.
type createInput struct {
	grantID      string
	sandboxID    string
	terms        terms
	gpuIDs       []string // GPU device ids leased to the subject
	cgroupParent string
}

// strSlice is Docker's StrSlice: a JSON string or a list of strings.
type strSlice []string

func (s *strSlice) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = strSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// The request types below are local copies of the Engine's container
// create body. The stdlib decoder matches keys case-insensitively and keeps
// the last duplicate, exactly like the Engine, so what is checked is what
// the Engine would have read. Only a freshly built request of allowlisted
// fields is forwarded; everything not listed here is dropped.

type healthConfig struct {
	Test        strSlice `json:",omitempty"`
	Interval    int64    `json:",omitempty"`
	Timeout     int64    `json:",omitempty"`
	StartPeriod int64    `json:",omitempty"`
	Retries     int      `json:",omitempty"`
}

type createIn struct {
	Hostname     string
	Domainname   string
	User         string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
	OpenStdin    bool
	StdinOnce    bool
	Env          []string
	Cmd          strSlice
	Healthcheck  *healthConfig
	ArgsEscaped  bool
	Image        string
	WorkingDir   string
	Entrypoint   strSlice
	Labels       map[string]string
	StopSignal   string
	StopTimeout  *int
	Shell        strSlice

	Volumes          json.RawMessage
	ExposedPorts     json.RawMessage
	NetworkDisabled  bool
	HostConfig       *hostConfigIn
	NetworkingConfig *struct {
		EndpointsConfig map[string]json.RawMessage
	}
}

type ulimit struct {
	Name string
	Soft int64
	Hard int64
}

type mountIn struct {
	Type         string
	Target       string
	ReadOnly     bool
	Source       string
	TmpfsOptions *struct {
		SizeBytes int64
		Mode      uint32
	}
}

type hostConfigIn struct {
	// Kept (and capped).
	NanoCpus          int64
	CpuShares         int64
	CpusetCpus        string
	Memory            int64
	MemoryReservation int64
	MemorySwap        int64
	PidsLimit         *int64
	Ulimits           []ulimit
	Tmpfs             map[string]string
	Mounts            []mountIn
	AutoRemove        bool
	RestartPolicy     struct {
		Name              string
		MaximumRetryCount int
	}
	OomScoreAdj    int
	OomKillDisable *bool
	ShmSize        int64
	Init           *bool
	ReadonlyRootfs bool
	CapDrop        []string
	ConsoleSize    [2]uint
	NetworkMode    string
	DeviceRequests []struct {
		Driver       string
		Count        int
		DeviceIDs    []string
		Capabilities [][]string
	}

	// Refused when set.
	Privileged        json.RawMessage
	PublishAllPorts   json.RawMessage
	CapAdd            json.RawMessage
	Binds             json.RawMessage
	VolumesFrom       json.RawMessage
	Links             json.RawMessage
	Devices           json.RawMessage
	DeviceCgroupRules json.RawMessage
	SecurityOpt       json.RawMessage
	Sysctls           json.RawMessage
	StorageOpt        json.RawMessage
	Annotations       json.RawMessage
	Isolation         json.RawMessage
	CgroupParent      json.RawMessage
	Runtime           json.RawMessage
	VolumeDriver      json.RawMessage
	PortBindings      json.RawMessage
	CpuQuota          json.RawMessage
	CpuPeriod         json.RawMessage
	PidMode           json.RawMessage
	IpcMode           json.RawMessage
	UTSMode           json.RawMessage
	UsernsMode        json.RawMessage
	CgroupnsMode      json.RawMessage
	Cgroup            json.RawMessage
	// Refused for any non-null value: an empty list unmasks /proc paths.
	MaskedPaths   json.RawMessage
	ReadonlyPaths json.RawMessage
}

type deviceRequestOut struct {
	Driver       string
	DeviceIDs    []string
	Capabilities [][]string
}

type mountOut struct {
	Type         string
	Target       string
	ReadOnly     bool `json:",omitempty"`
	TmpfsOptions struct {
		SizeBytes int64
		Mode      uint32 `json:",omitempty"`
	}
}

type hostConfigOut struct {
	NanoCpus          int64
	CpuShares         int64  `json:",omitempty"`
	CpusetCpus        string `json:",omitempty"`
	Memory            int64
	MemoryReservation int64 `json:",omitempty"`
	MemorySwap        int64
	PidsLimit         int64
	CgroupParent      string
	Ulimits           []ulimit          `json:",omitempty"`
	Tmpfs             map[string]string `json:",omitempty"`
	Mounts            []mountOut        `json:",omitempty"`
	AutoRemove        bool
	RestartPolicy     struct {
		Name              string
		MaximumRetryCount int
	}
	LogConfig struct {
		Type   string
		Config map[string]string
	}
	OomScoreAdj    int
	OomKillDisable bool
	ShmSize        int64 `json:",omitempty"`
	Init           *bool `json:",omitempty"`
	ReadonlyRootfs bool
	CapDrop        []string `json:",omitempty"`
	ConsoleSize    [2]uint
	NetworkMode    string
	DeviceRequests []deviceRequestOut `json:",omitempty"`
}

type createOut struct {
	Hostname     string `json:",omitempty"`
	Domainname   string `json:",omitempty"`
	User         string `json:",omitempty"`
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	Tty          bool
	OpenStdin    bool
	StdinOnce    bool
	Env          []string      `json:",omitempty"`
	Cmd          []string      `json:",omitempty"`
	Healthcheck  *healthConfig `json:",omitempty"`
	ArgsEscaped  bool          `json:",omitempty"`
	Image        string
	WorkingDir   string   `json:",omitempty"`
	Entrypoint   []string `json:",omitempty"`
	Labels       map[string]string
	StopSignal   string   `json:",omitempty"`
	StopTimeout  *int     `json:",omitempty"`
	Shell        []string `json:",omitempty"`
	HostConfig   hostConfigOut
}

// set reports whether a raw JSON value is present and not empty, false or
// zero.
func set(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return true // malformed counts as set: refused
	}
	switch x := v.(type) {
	case nil:
		return false
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	}
	return true
}

func nonNull(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// rewriteCreate checks a POST /containers/create body and builds the
// request that is forwarded. It returns the new body, the normalised image
// and a summary of the effective caps for the audit record.
func rewriteCreate(body []byte, in createInput) ([]byte, string, string, *apiError) {
	var c createIn
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, "", "", badRequest("container config is not valid: %v", err)
	}
	ref, ok := refAllowed(in.terms.images, c.Image)
	if !ok {
		return nil, c.Image, "", &apiError{status: http.StatusForbidden, msg: fmt.Sprintf("image %q is not allowed by the grant", c.Image)}
	}
	image := ref.String()
	if set(c.Volumes) {
		return nil, image, "", badRequest("volumes are not allowed")
	}
	if nc := c.NetworkingConfig; nc != nil {
		for name := range nc.EndpointsConfig {
			if name != "none" {
				return nil, image, "", badRequest("networks are not available to brokered containers (got %q)", name)
			}
		}
	}
	hin := c.HostConfig
	if hin == nil {
		hin = &hostConfigIn{}
	}
	hout, apiErr := buildHostConfig(hin, in)
	if apiErr != nil {
		return nil, image, "", apiErr
	}
	labels := make(map[string]string, len(c.Labels)+2)
	for k, v := range c.Labels {
		labels[k] = v
	}
	labels[LabelGrant] = in.grantID
	labels[LabelSandbox] = in.sandboxID
	out := createOut{
		Hostname: c.Hostname, Domainname: c.Domainname, User: c.User,
		AttachStdin: c.AttachStdin, AttachStdout: c.AttachStdout, AttachStderr: c.AttachStderr,
		Tty: c.Tty, OpenStdin: c.OpenStdin, StdinOnce: c.StdinOnce,
		Env: c.Env, Cmd: c.Cmd, Healthcheck: c.Healthcheck, ArgsEscaped: c.ArgsEscaped,
		Image: c.Image, WorkingDir: c.WorkingDir, Entrypoint: c.Entrypoint, Labels: labels,
		StopSignal: c.StopSignal, StopTimeout: c.StopTimeout, Shell: c.Shell,
		HostConfig: *hout,
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, image, "", badRequest("cannot encode container config")
	}
	caps := fmt.Sprintf("caps nano_cpus=%d memory=%d pids=%d cgroup_parent=%s gpus=%d",
		hout.NanoCpus, hout.Memory, hout.PidsLimit, hout.CgroupParent, len(in.gpuIDs)*len(hout.DeviceRequests))
	return enc, image, caps, nil
}

func buildHostConfig(h *hostConfigIn, in createInput) (*hostConfigOut, *apiError) {
	for name, raw := range map[string]json.RawMessage{
		"Privileged": h.Privileged, "PublishAllPorts": h.PublishAllPorts, "CapAdd": h.CapAdd,
		"Binds": h.Binds, "VolumesFrom": h.VolumesFrom, "Links": h.Links, "Devices": h.Devices,
		"DeviceCgroupRules": h.DeviceCgroupRules, "SecurityOpt": h.SecurityOpt, "Sysctls": h.Sysctls,
		"StorageOpt": h.StorageOpt, "Annotations": h.Annotations, "Isolation": h.Isolation,
		"CgroupParent": h.CgroupParent, "Runtime": h.Runtime, "VolumeDriver": h.VolumeDriver,
		"PortBindings": h.PortBindings, "CpuQuota": h.CpuQuota, "CpuPeriod": h.CpuPeriod,
		"PidMode": h.PidMode, "IpcMode": h.IpcMode, "UTSMode": h.UTSMode, "UsernsMode": h.UsernsMode,
		"CgroupnsMode": h.CgroupnsMode, "Cgroup": h.Cgroup,
	} {
		if set(raw) {
			return nil, badRequest("%s is not allowed", name)
		}
	}
	if nonNull(h.MaskedPaths) || nonNull(h.ReadonlyPaths) {
		return nil, badRequest("MaskedPaths and ReadonlyPaths are not allowed")
	}
	if h.NetworkMode != "" && h.NetworkMode != "none" {
		return nil, badRequest("network mode %q is not allowed; brokered containers have no network", h.NetworkMode)
	}
	if h.OomScoreAdj < 0 {
		return nil, badRequest("a negative OomScoreAdj is not allowed")
	}
	if h.OomKillDisable != nil && *h.OomKillDisable {
		return nil, badRequest("OomKillDisable is not allowed")
	}
	o := &hostConfigOut{
		CpuShares: h.CpuShares, CpusetCpus: h.CpusetCpus, MemoryReservation: h.MemoryReservation,
		AutoRemove: h.AutoRemove, OomScoreAdj: h.OomScoreAdj, Init: h.Init,
		ReadonlyRootfs: h.ReadonlyRootfs, CapDrop: h.CapDrop, ConsoleSize: h.ConsoleSize,
		NetworkMode: "none", CgroupParent: in.cgroupParent,
	}
	o.LogConfig.Type = "local"
	o.LogConfig.Config = map[string]string{"max-size": "10m", "max-file": "3"}

	cpus := in.terms.nanoCPUs
	if cpus <= 0 {
		cpus = defaultNanoCPUs
	}
	o.NanoCpus = h.NanoCpus
	if o.NanoCpus <= 0 || o.NanoCpus > cpus {
		o.NanoCpus = cpus
	}
	mem := in.terms.memory
	if mem <= 0 {
		mem = defaultMemory
	}
	o.Memory = h.Memory
	if o.Memory <= 0 || o.Memory > mem {
		o.Memory = mem
	}
	o.MemorySwap = o.Memory // no swap beyond the memory cap
	if o.MemoryReservation > o.Memory {
		o.MemoryReservation = o.Memory
	}
	o.PidsLimit = maxPids
	if h.PidsLimit != nil && *h.PidsLimit > 0 && *h.PidsLimit < maxPids {
		o.PidsLimit = *h.PidsLimit
	}
	o.ShmSize = min(max(h.ShmSize, 0), maxShmSize)

	switch h.RestartPolicy.Name {
	case "", "no":
		o.RestartPolicy.Name = "no"
	case "on-failure":
		o.RestartPolicy.Name = "on-failure"
		n := h.RestartPolicy.MaximumRetryCount
		if n <= 0 || n > maxRestartRetry {
			n = maxRestartRetry // 0 would mean unlimited
		}
		o.RestartPolicy.MaximumRetryCount = n
	default:
		return nil, badRequest("restart policy %q is not allowed; use no or on-failure", h.RestartPolicy.Name)
	}

	for _, u := range h.Ulimits {
		switch u.Name {
		case "nofile":
			u.Soft, u.Hard = clampLimit(u.Soft, maxNofile), clampLimit(u.Hard, maxNofile)
		case "nproc":
			u.Soft, u.Hard = clampLimit(u.Soft, maxNproc), clampLimit(u.Hard, maxNproc)
		case "memlock", "core":
			u.Soft, u.Hard = 0, 0
		default:
			return nil, badRequest("ulimit %q is not allowed", u.Name)
		}
		o.Ulimits = append(o.Ulimits, u)
	}

	if len(h.Tmpfs) > 0 {
		o.Tmpfs = make(map[string]string, len(h.Tmpfs))
		for path, opts := range h.Tmpfs {
			fixed, err := tmpfsOptions(opts)
			if err != nil {
				return nil, err
			}
			o.Tmpfs[path] = fixed
		}
	}
	for _, m := range h.Mounts {
		if m.Type != "tmpfs" {
			return nil, badRequest("mounts of type %q are not allowed; only tmpfs", m.Type)
		}
		if m.Source != "" {
			return nil, badRequest("tmpfs mounts take no source")
		}
		mo := mountOut{Type: "tmpfs", Target: m.Target, ReadOnly: m.ReadOnly}
		mo.TmpfsOptions.SizeBytes = maxTmpfsSize
		if t := m.TmpfsOptions; t != nil {
			if t.SizeBytes > maxTmpfsSize {
				return nil, badRequest("tmpfs mounts are limited to 1 GiB")
			}
			if t.SizeBytes > 0 {
				mo.TmpfsOptions.SizeBytes = t.SizeBytes
			}
			mo.TmpfsOptions.Mode = t.Mode
		}
		o.Mounts = append(o.Mounts, mo)
	}

	if len(h.DeviceRequests) > 0 {
		if !in.terms.gpu {
			return nil, badRequest("GPU device requests are not allowed by the grant")
		}
		if len(in.gpuIDs) == 0 {
			return nil, badRequest("no GPU is leased to this subject")
		}
		for _, dr := range h.DeviceRequests {
			if dr.Driver != "" && dr.Driver != "nvidia" {
				return nil, badRequest("device driver %q is not allowed", dr.Driver)
			}
			if dr.Count != 0 {
				return nil, badRequest("device request Count is not allowed; the leased GPUs are used")
			}
		}
		o.DeviceRequests = []deviceRequestOut{{Driver: "nvidia", DeviceIDs: in.gpuIDs, Capabilities: [][]string{{"gpu"}}}}
	}
	return o, nil
}

func clampLimit(v, limit int64) int64 {
	if v <= 0 || v > limit {
		return limit
	}
	return v
}

// tmpfsOptions checks a --tmpfs option string and adds size=1g when absent.
func tmpfsOptions(opts string) (string, *apiError) {
	var parts []string
	sized := false
	for _, o := range strings.Split(opts, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if v, ok := strings.CutPrefix(o, "size="); ok {
			n, err := parseSize(v)
			if err != nil || n <= 0 || n > maxTmpfsSize {
				return "", badRequest("tmpfs size %q is not allowed; at most 1 GiB", v)
			}
			sized = true
		}
		parts = append(parts, o)
	}
	if !sized {
		parts = append(parts, "size=1g")
	}
	return strings.Join(parts, ","), nil
}

// parseSize reads a tmpfs size such as 512m, 1g or 65536.
func parseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}
