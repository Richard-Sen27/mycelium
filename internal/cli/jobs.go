package cli

// Phase 4: jobs in sandboxes and profiles (hub API).
//
//	mycel job run --profile <name> [--node] [--channel] [--timeout] [--input art_…=path]... [--output glob]...
//	              [--workspace art_…|--git url[@ref]] [--env K=V]... -- <command...>
//	mycel job ls [--all] | get <id> | logs <id> [--follow] | cancel <id>
//	mycel profiles ls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	mycelv1 "github.com/richard-sen27/mycelium/gen/mycel/v1"
	mcpadapter "github.com/richard-sen27/mycelium/internal/adapters/mcp"
)

func (a *app) jobCmd() *cobra.Command {
	cmd := groupCmd("job", "Run and inspect jobs in sandboxes", "jobs")
	cmd.AddCommand(a.jobRunCmd(), a.jobLsCmd(), a.jobGetCmd(), a.jobLogsCmd(), a.jobCancelCmd())
	return cmd
}

// jobRunFlags are the flags of `job run` before they become a JobSpec.
type jobRunFlags struct {
	profile, node, channel, timeout, name string
	inputs, outputs, env                  []string
	workspace, git                        string
}

// parseGitSource splits "url[@ref]". The ref is taken after the last "@"
// only when that "@" lies in the URL's path, so user info
// ("https://user@host/…", "git@host:…") is never mistaken for a ref.
func parseGitSource(s string) (*mycelv1.GitSource, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, usagef("--git needs a repository URL")
	}
	pathStart := -1
	if i := strings.Index(s, "://"); i >= 0 {
		if j := strings.IndexByte(s[i+3:], '/'); j >= 0 {
			pathStart = i + 3 + j
		}
	} else if i := strings.IndexByte(s, ':'); i >= 0 {
		pathStart = i // scp-like: [user@]host:path
	}
	at := strings.LastIndexByte(s, '@')
	if pathStart < 0 || at < pathStart {
		return &mycelv1.GitSource{Url: s}, nil
	}
	u, ref := s[:at], s[at+1:]
	if ref == "" {
		return nil, usagef("--git %q: empty ref after @", s)
	}
	return &mycelv1.GitSource{Url: u, Ref: ref}, nil
}

// parseJobInput parses "art_…[=path]".
func parseJobInput(s string) (*mycelv1.JobInput, error) {
	id, path, _ := strings.Cut(s, "=")
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "art_") || len(id) == len("art_") {
		return nil, usagef("--input %q: want art_<id>[=path]", s)
	}
	return &mycelv1.JobInput{Artifact: id, Path: strings.TrimSpace(path)}, nil
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseJobEnv parses repeated "K=V" into a map; MYCEL_* keys are refused.
func parseJobEnv(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(items))
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || !envKeyRE.MatchString(k) {
			return nil, usagef("--env %q: want KEY=VALUE with KEY of letters, digits and _ (not starting with a digit)", it)
		}
		if strings.ContainsRune(v, 0) {
			return nil, usagef("--env %s: the value contains a NUL byte", k)
		}
		if strings.HasPrefix(strings.ToUpper(k), "MYCEL_") {
			return nil, usagef("--env %s: MYCEL_* variables are reserved", k)
		}
		if _, dup := env[k]; dup {
			return nil, usagef("--env %s given twice", k)
		}
		env[k] = v
	}
	return env, nil
}

// buildJobSpec turns `job run` flags and the command into a JobSpec.
func buildJobSpec(f jobRunFlags, command []string) (*mycelv1.JobSpec, error) {
	if strings.TrimSpace(f.profile) == "" {
		return nil, usagef("--profile is required")
	}
	if len(command) == 0 || command[0] == "" {
		return nil, usagef("give the command after --, e.g. mycel job run --profile dev -- make test")
	}
	if f.workspace != "" && f.git != "" {
		return nil, usagef("--workspace and --git cannot be combined")
	}
	if f.timeout != "" {
		if d, err := time.ParseDuration(f.timeout); err != nil || d <= 0 {
			return nil, usagef("invalid --timeout %q: want a duration such as 30m", f.timeout)
		}
	}
	spec := &mycelv1.JobSpec{
		Profile: strings.TrimSpace(f.profile), Command: command, Timeout: f.timeout,
		Node: f.node, Name: f.name, Outputs: f.outputs,
	}
	if f.channel != "" {
		spec.Channel = normChannel(f.channel)
	}
	switch {
	case f.workspace != "":
		if !strings.HasPrefix(f.workspace, "art_") {
			return nil, usagef("--workspace %q: want an artifact id art_…", f.workspace)
		}
		spec.Workspace = &mycelv1.WorkspaceSeed{Source: &mycelv1.WorkspaceSeed_Artifact{Artifact: f.workspace}}
	case f.git != "":
		g, err := parseGitSource(f.git)
		if err != nil {
			return nil, err
		}
		spec.Workspace = &mycelv1.WorkspaceSeed{Source: &mycelv1.WorkspaceSeed_Git{Git: g}}
	}
	for _, in := range f.inputs {
		ji, err := parseJobInput(in)
		if err != nil {
			return nil, err
		}
		spec.Inputs = append(spec.Inputs, ji)
	}
	env, err := parseJobEnv(f.env)
	if err != nil {
		return nil, err
	}
	spec.Env = env
	return spec, nil
}

func (a *app) jobRunCmd() *cobra.Command {
	var f jobRunFlags
	cmd := &cobra.Command{
		Use:   "run --profile <name> [flags] -- <command...>",
		Short: "Run a command in a fresh sandbox on a node that offers the profile",
		Example: "  mycel job run --profile dev -- go test ./...\n" +
			"  mycel job run --profile dev --git https://github.com/org/repo@main --output 'dist/*' -- make build\n" +
			"  mycel job run --profile default --input art_01J…=data.csv --env MODE=fast -- python run.py data.csv",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if d := cmd.ArgsLenAtDash(); d > 0 {
				return usagef("unexpected argument %q before --", args[0])
			}
			spec, err := buildJobSpec(f, args)
			if err != nil {
				return err
			}
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.jobs.RunJob(cmd.Context(), connect.NewRequest(&mycelv1.RunJobRequest{Spec: spec}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			j := resp.Msg.GetJob()
			where := ""
			if j.GetNode() != "" {
				where = " on " + cellText(j.GetNode())
			}
			_, err = fmt.Fprintf(a.stdout, "Job %s %s%s (profile %s).\nFollow it with: mycel job logs %s --follow\n",
				cellText(j.GetId()), mcpadapter.JobStateName(j.GetState()), where, cellText(spec.GetProfile()), cellText(j.GetId()))
			return err
		},
	}
	// Everything after the first positional argument (or after --) is the
	// command, so its own flags are not parsed as ours.
	cmd.Flags().SetInterspersed(false)
	fl := cmd.Flags()
	fl.StringVar(&f.profile, "profile", "", "sandbox profile (see mycel profiles ls)")
	fl.StringVar(&f.node, "node", "", "run on this node only")
	fl.StringVar(&f.channel, "channel", "", "post the result message to this channel")
	fl.StringVar(&f.timeout, "timeout", "", "wall-clock limit, e.g. 30m (capped by the profile; default the profile's)")
	fl.StringVar(&f.name, "name", "", "human label for the job")
	fl.StringArrayVar(&f.inputs, "input", nil, "input artifact art_…[=path relative to /workspace] (repeatable)")
	fl.StringArrayVar(&f.outputs, "output", nil, "glob relative to /workspace collected as an artifact (repeatable)")
	fl.StringVar(&f.workspace, "workspace", "", "art_… tarball extracted into /workspace")
	fl.StringVar(&f.git, "git", "", "git repository cloned into /workspace: url[@ref]")
	fl.StringArrayVar(&f.env, "env", nil, "sandbox environment variable KEY=VALUE (repeatable)")
	return cmd
}

func (a *app) jobLsCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "ls [--all]",
		Aliases: []string{"list"},
		Short:   "List jobs (running and queued; --all adds finished ones)",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.jobs.ListJobs(cmd.Context(), connect.NewRequest(&mycelv1.ListJobsRequest{IncludeFinished: all}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "ID", "NAME", "PROFILE", "STATE", "NODE", "EXIT", "SUBMITTED BY", "CREATED")
			for _, j := range resp.Msg.GetJobs() {
				row(tw, j.GetId(), dash(j.GetSpec().GetName()), dash(j.GetSpec().GetProfile()), mcpadapter.JobStateName(j.GetState()),
					dash(j.GetNode()), exitCell(j), dash(j.GetSubmittedBy()), ago(j.GetCreatedAt(), a.now()))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include finished jobs")
	return cmd
}

func exitCell(j *mycelv1.Job) string {
	if !mcpadapter.JobFinished(j.GetState()) {
		return "-"
	}
	return strconv.Itoa(int(j.GetExitCode()))
}

func (a *app) jobGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show a job",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.jobs.GetJob(cmd.Context(), connect.NewRequest(&mycelv1.GetJobRequest{Id: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			return a.printJob(resp.Msg.GetJob())
		},
	}
}

func (a *app) printJob(j *mycelv1.Job) error {
	sp := j.GetSpec()
	tw := newTable(a.stdout)
	kv := func(k, v string) { row(tw, k+":", v) }
	kv("ID", j.GetId())
	kv("Name", dash(sp.GetName()))
	kv("State", mcpadapter.JobStateName(j.GetState()))
	kv("Profile", dash(sp.GetProfile()))
	kv("Command", dash(mcpadapter.CommandText(sp.GetCommand())))
	switch w := sp.GetWorkspace(); {
	case w.GetArtifact() != "":
		kv("Workspace", w.GetArtifact())
	case w.GetGit() != nil:
		g := w.GetGit().GetUrl()
		if r := w.GetGit().GetRef(); r != "" {
			g += "@" + r
		}
		kv("Workspace", g)
	}
	for _, in := range sp.GetInputs() {
		kv("Input", in.GetArtifact()+" -> "+dash(in.GetPath()))
	}
	if len(sp.GetOutputs()) > 0 {
		kv("Outputs", strings.Join(sp.GetOutputs(), ", "))
	}
	kv("Node", dash(j.GetNode()))
	kv("Backend", dash(j.GetBackend()))
	kv("Sandbox", dash(j.GetSandboxId()))
	kv("Exit code", exitCell(j))
	kv("Error", dash(j.GetError()))
	kv("Artifacts", dash(strings.Join(j.GetArtifacts(), ", ")))
	kv("Channel", dash(sp.GetChannel()))
	kv("Submitted by", dash(j.GetSubmittedBy()))
	kv("Owner", dash(j.GetOwnerId()))
	kv("Created", dash(j.GetCreatedAt()))
	kv("Started", dash(j.GetStats().GetStartedAt()))
	kv("Finished", dash(j.GetStats().GetFinishedAt()))
	kv("Output", bytesString(j.GetLogBytes()))
	return tw.Flush()
}

func (a *app) jobLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <id> [--follow]",
		Short: "Print a job's output; --follow keeps streaming until the job ends",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.jobLogs(ctx, hc, args[0], follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new output until the job ends")
	return cmd
}

// Reconnect backoff of `job logs --follow`; tests shorten logRetryBase.
var (
	logRetryBase = 500 * time.Millisecond
	logRetryMax  = 10 * time.Second
)

// retryableStreamError reports errors after which a log stream is redialled:
// transport failures and transient Connect codes. Refusals (not found,
// permission, authentication, bad request) are final.
func retryableStreamError(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return true
	}
	switch ce.Code() {
	case connect.CodeUnavailable, connect.CodeUnknown, connect.CodeDeadlineExceeded, connect.CodeAborted,
		connect.CodeResourceExhausted, connect.CodeInternal, connect.CodeCanceled:
		return true
	}
	return false
}

// logPrinter prints log chunks in byte order: overlap with what was already
// printed is skipped, a jump forward is marked with a gap line, and a
// character split across chunks is printed whole.
type logPrinter struct {
	a     *app
	next  uint64 // offset of the next byte expected
	carry []byte
}

func (p *logPrinter) chunk(m *mycelv1.StreamLogsResponse) error {
	data, off := m.GetData(), m.GetOffset()
	if off < p.next {
		if skip := p.next - off; skip >= uint64(len(data)) {
			data = nil
		} else {
			data = data[skip:]
		}
		off = p.next
	}
	if off > p.next {
		if err := p.flush(); err != nil {
			return err
		}
		if !p.a.jsonOut {
			if _, err := fmt.Fprintf(p.a.stdout, "\n[gap: %d bytes of output missing]\n", off-p.next); err != nil {
				return err
			}
		}
		p.next = off
	}
	p.next += uint64(len(data))
	if p.a.jsonOut {
		if len(data) == 0 && !m.GetEof() {
			return nil
		}
		return p.a.printProto(&mycelv1.StreamLogsResponse{Data: data, Offset: off, Eof: m.GetEof()})
	}
	p.carry = append(p.carry, data...)
	n := completeUTF8(p.carry)
	if m.GetEof() {
		n = len(p.carry)
	}
	if n > 0 {
		if _, err := fmt.Fprint(p.a.stdout, termText(string(p.carry[:n]))); err != nil {
			return err
		}
	}
	p.carry = append([]byte(nil), p.carry[n:]...)
	return nil
}

func (p *logPrinter) flush() error {
	if len(p.carry) == 0 {
		return nil
	}
	_, err := fmt.Fprint(p.a.stdout, termText(string(p.carry)))
	p.carry = nil
	return err
}

// jobLogs prints a job's output. Without follow it reads one stream. With
// follow it redials StreamLogs from the next unseen byte after a dropped
// stream or one closed without eof (backoff doubling up to logRetryMax), and
// stops on eof, when GetJob shows the job finished with all its output
// printed, or when ctx ends.
func (a *app) jobLogs(ctx context.Context, hc *hubClient, id string, follow bool) error {
	p := &logPrinter{a: a}
	defer func() { _ = p.flush() }()
	backoff := logRetryBase
	for {
		eof, progressed := false, false
		stream, err := hc.jobStream.StreamLogs(ctx, connect.NewRequest(&mycelv1.StreamLogsRequest{Id: id, FromByte: p.next, Follow: follow}))
		if err == nil {
			for !eof && stream.Receive() {
				m := stream.Msg()
				progressed = progressed || len(m.GetData()) > 0
				if err := p.chunk(m); err != nil {
					_ = stream.Close()
					return err
				}
				eof = m.GetEof()
			}
			err = stream.Err()
			_ = stream.Close()
		}
		switch {
		case eof || ctx.Err() != nil:
			return nil
		case !follow && err != nil:
			return rpcError(hc.url, err)
		case !follow:
			return nil
		case err != nil && !retryableStreamError(err):
			return rpcError(hc.url, err)
		}
		// Dropped, or closed without eof while following: is the job over?
		resp, gerr := hc.jobs.GetJob(ctx, connect.NewRequest(&mycelv1.GetJobRequest{Id: id}))
		switch {
		case ctx.Err() != nil:
			return nil
		case gerr != nil && !retryableStreamError(gerr):
			return rpcError(hc.url, gerr)
		case gerr == nil && mcpadapter.JobFinished(resp.Msg.GetJob().GetState()) && p.next >= resp.Msg.GetJob().GetLogBytes():
			return nil
		}
		if progressed {
			backoff = logRetryBase
		}
		a.log.Debug("job log stream ended early; reconnecting", "job", id, "from_byte", p.next, "err", err, "after", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, logRetryMax)
	}
}

// completeUTF8 returns the length of b without a trailing incomplete UTF-8
// sequence, so a character split across log chunks is printed whole.
func completeUTF8(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

func (a *app) jobCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a queued or running job",
		Args:  exactArgs(1, "id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.jobs.CancelJob(cmd.Context(), connect.NewRequest(&mycelv1.CancelJobRequest{Id: args[0]}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			_, err = fmt.Fprintf(a.stdout, "Job %s %s.\n", cellText(args[0]), mcpadapter.JobStateName(resp.Msg.GetJob().GetState()))
			return err
		},
	}
}

func (a *app) profilesCmd() *cobra.Command {
	cmd := groupCmd("profiles", "List sandbox profiles", "profile")
	cmd.AddCommand(&cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the hub's sandbox profiles",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc, err := a.hub(cmd)
			if err != nil {
				return err
			}
			resp, err := hc.jobs.ListProfiles(cmd.Context(), connect.NewRequest(&mycelv1.ListProfilesRequest{}))
			if err != nil {
				return rpcError(hc.url, err)
			}
			if a.jsonOut {
				return a.printProto(resp.Msg)
			}
			tw := newTable(a.stdout)
			row(tw, "NAME", "VERSION", "BACKENDS", "CPUS", "MEMORY", "WALL CLOCK", "EGRESS", "BROKERS", "DESCRIPTION")
			for _, p := range resp.Msg.GetProfiles() {
				l := p.GetLimits()
				cpus := "-"
				if l.GetCpus() > 0 {
					cpus = strconv.Itoa(int(l.GetCpus()))
				}
				mem := "-"
				if l.GetMemoryBytes() > 0 {
					mem = bytesString(uint64(l.GetMemoryBytes())) //nolint:gosec // positive, checked above
				}
				egress := "none"
				if allow := p.GetEgress().GetAllow(); len(allow) > 0 {
					egress = strings.Join(allow, ",")
				}
				row(tw, p.GetName(), strconv.Itoa(int(p.GetVersion())), dash(strings.Join(p.GetBackends(), ",")), cpus, mem,
					dash(l.GetWallClock()), egress, dash(strings.Join(p.GetBrokers(), ",")), dash(p.GetDescription()))
			}
			return tw.Flush()
		},
	})
	return cmd
}
