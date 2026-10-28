package docker

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/richard-sen27/mycelium/internal/broker"
)

var versionRe = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

// containerActions are the per-container endpoints a grant may use, with
// their method.
var containerActions = map[string]string{
	"json":   http.MethodGet,
	"start":  http.MethodPost,
	"stop":   http.MethodPost,
	"kill":   http.MethodPost,
	"wait":   http.MethodPost,
	"logs":   http.MethodGet,
	"attach": http.MethodPost,
	"resize": http.MethodPost, // terminal size for attach
	"exec":   http.MethodPost,
}

// execActions are the per-exec endpoints, with their method.
var execActions = map[string]string{
	"start":  http.MethodPost,
	"json":   http.MethodGet,
	"resize": http.MethodPost,
}

// call is one authorised request on its way to the Engine.
type call struct {
	op           string // e.g. "containers.create"
	target       string
	path         string // Engine path, version prefix included
	query        string
	body         []byte // replaces the request body when non-nil
	filterImages bool
	recordExec   bool
	rules        []imageRule
	note         string // audited with the request: effective caps, exec argv
	unlock       func() // released after the request (per-grant create lock)

	g      *grant
	status int
	err    error
}

type callKey struct{}

func (b *Broker) serve(g *grant, w http.ResponseWriter, r *http.Request) {
	rec := broker.Record{Broker: broker.KindDockerHost, GrantID: g.id, Op: r.Method}
	b.mu.Lock()
	rec.Subject, rec.SandboxID = g.subject, g.sandboxID
	t, expires, frozen, reason, current := g.terms, g.expires, b.frozen, b.reason, b.grants[g.id] == g && !g.stopping
	var reqID uint64
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if current {
		// Detach waits for every handler that got this far.
		g.wg.Add(1)
		defer g.wg.Done()
		reqID = g.nextReq
		g.nextReq++
		g.inflight[reqID] = cancel
		defer func() {
			b.mu.Lock()
			delete(g.inflight, reqID)
			b.mu.Unlock()
		}()
	}
	b.mu.Unlock()

	p := r.URL.Path
	if g.prefix != "" {
		if len(p) <= len(g.prefix) || subtle.ConstantTimeCompare([]byte(p[:len(g.prefix)]), []byte(g.prefix)) != 1 || p[len(g.prefix)] != '/' {
			rec.Op = r.Method + " (no credential)"
			b.deny(w, rec, &apiError{status: http.StatusForbidden, msg: "missing or wrong path credential"})
			return
		}
		p = p[len(g.prefix):]
	}
	rec.Op = r.Method + " " + truncate(p, 64)
	switch {
	case !current:
		b.deny(w, rec, &apiError{status: http.StatusServiceUnavailable, msg: "grant no longer active"})
		return
	case frozen:
		b.deny(w, rec, &apiError{status: http.StatusServiceUnavailable, msg: "broker frozen: " + reason})
		return
	case b.ceiling != nil && !b.ceiling.Offers(OfferName):
		b.deny(w, rec, &apiError{status: http.StatusServiceUnavailable, msg: "docker sharing is switched off on this node"})
		return
	case b.ceiling != nil && !b.ceiling.Active():
		b.deny(w, rec, &apiError{status: http.StatusServiceUnavailable, msg: "sharing is not active on this node right now"})
		return
	case !expires.IsZero() && !b.now().Before(expires):
		b.deny(w, rec, &apiError{status: http.StatusForbidden, msg: "grant expired"})
		return
	}
	if r.URL.RawPath != "" || path.Clean(p) != p {
		b.deny(w, rec, badRequest("unclean request path"))
		return
	}
	version := versionRe.FindString(p)
	rest := p[len(version):]

	c, apiErr := b.authorize(ctx, g, t, r, version, rest)
	if c != nil {
		rec.Op = r.Method + " " + c.op
		rec.Target = c.target
	}
	if c != nil && c.unlock != nil {
		defer c.unlock()
	}
	if apiErr != nil {
		b.deny(w, rec, apiErr)
		return
	}
	c.g, c.rules = g, t.images

	out := r.WithContext(context.WithValue(ctx, callKey{}, c))
	if c.body != nil {
		out.Body = io.NopCloser(bytes.NewReader(c.body))
		out.ContentLength = int64(len(c.body))
		out.Header = r.Header.Clone()
		out.Header.Set("Content-Length", fmt.Sprint(len(c.body)))
		if len(c.body) == 0 {
			out.Body = http.NoBody
			out.Header.Del("Content-Type")
		}
	}
	b.mu.Lock()
	g.dReq++
	b.mu.Unlock()
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		// The proxy panics with http.ErrAbortHandler when a stream is cut
		// (client gone, Detach, Freeze); the request is audited either way.
		p := recover()
		rec.Allowed = true
		switch {
		case p != nil:
			rec.Reason = "aborted"
		case c.err != nil:
			rec.Reason = "engine: " + c.err.Error()
		case c.status >= 400:
			rec.Reason = fmt.Sprintf("engine status %d", c.status)
		}
		if c.note != "" {
			rec.Reason = strings.TrimSpace(c.note + " " + rec.Reason)
		}
		b.log.Info("docker request", "grant", g.id, "op", rec.Op, "target", rec.Target, "status", c.status, "reason", rec.Reason)
		b.emit(rec)
		if p != nil {
			panic(p)
		}
	}()
	b.proxy.ServeHTTP(sw, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// authorize maps a request onto an allowed call or a refusal.
func (b *Broker) authorize(ctx context.Context, g *grant, t terms, r *http.Request, version, rest string) (*call, *apiError) {
	segs := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	m := r.Method
	c := &call{op: "unknown", path: version + rest, query: r.URL.RawQuery}
	forbidden := &apiError{status: http.StatusForbidden, msg: "endpoint not allowed by the grant"}
	is := func(method string, n int) bool {
		return len(segs) == n && (m == method || (method == http.MethodGet && m == http.MethodHead))
	}

	switch segs[0] {
	case "_ping":
		c.op = "system.ping"
		if is(http.MethodGet, 1) {
			return c, nil
		}
	case "version":
		c.op = "system.version"
		if is(http.MethodGet, 1) {
			return c, nil
		}
	case "images":
		return b.authorizeImage(c, g, t, r, segs, is)
	case "containers":
		return b.authorizeContainer(ctx, c, g, t, r, version, segs, is)
	case "exec":
		c.op = "exec"
		if len(segs) == 3 {
			c.op = "exec." + segs[2]
			c.target = segs[1]
			want, ok := execActions[segs[2]]
			if !ok || !is(want, 3) {
				return c, forbidden
			}
			b.mu.Lock()
			mine := g.execs[segs[1]]
			b.mu.Unlock()
			if !mine {
				return c, &apiError{status: http.StatusNotFound, msg: "No such exec instance: " + segs[1]}
			}
			return c, nil
		}
	default:
		c.op = segs[0]
	}
	return c, forbidden
}

func (b *Broker) authorizeImage(c *call, g *grant, t terms, r *http.Request, segs []string, is func(string, int) bool) (*call, *apiError) {
	switch {
	case is(http.MethodGet, 2) && segs[1] == "json":
		c.op, c.filterImages = "images.list", true
		return c, nil
	case is(http.MethodPost, 2) && segs[1] == "create":
		c.op = "images.pull"
		q := r.URL.Query()
		if q.Get("fromSrc") != "" || q.Get("repo") != "" || q.Get("changes") != "" {
			return c, &apiError{status: http.StatusForbidden, msg: "image import is not allowed"}
		}
		ref := q.Get("fromImage")
		c.target = ref
		if tag := q.Get("tag"); tag != "" {
			if strings.HasPrefix(tag, "sha256:") {
				ref += "@" + tag
			} else {
				ref += ":" + tag
			}
		}
		parsed, explicit, ok := parseRef(ref, true)
		if !ok {
			return c, badRequest("invalid image reference %q", ref)
		}
		c.target = parsed.String()
		if !imageAllowed(t.images, parsed) {
			return c, &apiError{status: http.StatusForbidden, msg: fmt.Sprintf("image %q is not allowed by the grant", ref)}
		}
		b.mu.Lock()
		ok = g.pulls.allow(b.now(), pullsPerMinute/60.0, pullsPerMinute)
		b.mu.Unlock()
		if !ok {
			return c, &apiError{status: http.StatusTooManyRequests, msg: fmt.Sprintf("pull rate limit: %d per minute", pullsPerMinute)}
		}
		if !explicit {
			// An empty tag would pull every tag of the repository.
			q.Set("tag", "latest")
			c.query = q.Encode()
		}
		return c, nil
	case len(segs) >= 3 && segs[len(segs)-1] == "json" && is(http.MethodGet, len(segs)):
		c.op = "images.inspect"
		name := strings.Join(segs[1:len(segs)-1], "/")
		c.target = name
		ref, ok := refAllowed(t.images, name)
		if !ok {
			return c, &apiError{status: http.StatusForbidden, msg: fmt.Sprintf("image %q is not allowed by the grant", name)}
		}
		c.target = ref.String()
		return c, nil
	}
	c.op = "images"
	if len(segs) > 1 {
		c.op = "images." + segs[len(segs)-1]
	}
	return c, &apiError{status: http.StatusForbidden, msg: "endpoint not allowed by the grant"}
}

func (b *Broker) authorizeContainer(ctx context.Context, c *call, g *grant, t terms, r *http.Request, version string,
	segs []string, is func(string, int) bool,
) (*call, *apiError) {
	forbidden := &apiError{status: http.StatusForbidden, msg: "endpoint not allowed by the grant"}
	label := LabelGrant + "=" + g.id
	switch {
	case len(segs) == 2 && (segs[1] == "json" || segs[1] == "prune"):
		c.op = "containers.list"
		if segs[1] == "prune" {
			c.op = "containers.prune"
			if !is(http.MethodPost, 2) {
				return c, forbidden
			}
		} else if !is(http.MethodGet, 2) {
			return c, forbidden
		}
		q := r.URL.Query()
		f, err := addLabelFilter(q.Get("filters"), label)
		if err != nil {
			return c, badRequest("invalid filters: %v", err)
		}
		q.Set("filters", f)
		c.query = q.Encode()
		return c, nil
	case len(segs) == 2 && segs[1] == "create":
		c.op = "containers.create"
		if !is(http.MethodPost, 2) {
			return c, forbidden
		}
		body, apiErr := readBody(r)
		if apiErr != nil {
			return c, apiErr
		}
		b.mu.Lock()
		in := createInput{grantID: g.id, sandboxID: g.sandboxID, terms: t}
		subject := g.subject
		b.mu.Unlock()
		if t.gpu && b.gpuDevices != nil {
			in.gpuIDs = b.gpuDevices(subject, in.sandboxID)
		}
		parent, err := b.cgroupParent(ctx, g.id)
		if err != nil {
			return c, &apiError{status: http.StatusBadGateway, msg: "docker engine unavailable", reason: "cgroup driver: " + err.Error()}
		}
		in.cgroupParent = parent
		out, image, caps, apiErr := rewriteCreate(body, in)
		c.target = image
		if apiErr != nil {
			return c, apiErr
		}
		// Creates of one grant are serialised so the container cap holds.
		g.createMu.Lock()
		c.unlock = g.createMu.Unlock
		ids, err := b.grantContainers(ctx, g.id, false)
		if err != nil {
			return c, &apiError{status: http.StatusBadGateway, msg: "docker engine unavailable", reason: "count containers: " + err.Error()}
		}
		if len(ids) >= maxContainers {
			return c, &apiError{status: http.StatusConflict, msg: fmt.Sprintf("the grant already has %d containers (limit %d)", len(ids), maxContainers)}
		}
		c.body, c.note = out, caps
		return c, nil
	case len(segs) == 2 && r.Method == http.MethodDelete:
		c.op, c.target = "containers.remove", segs[1]
		id, apiErr := b.owned(ctx, g, segs[1])
		if apiErr != nil {
			return c, apiErr
		}
		c.target, c.path = id, version+"/containers/"+id
		return c, nil
	case len(segs) == 3:
		action := segs[2]
		c.op, c.target = "containers."+action, segs[1]
		want, ok := containerActions[action]
		if !ok || !is(want, 3) {
			if action == "json" {
				c.op = "containers.inspect"
			}
			return c, forbidden
		}
		if action == "json" {
			c.op = "containers.inspect"
		}
		switch action {
		case "exec":
			body, apiErr := readBody(r)
			if apiErr != nil {
				return c, apiErr
			}
			out, argv, apiErr := rewriteExec(body)
			if apiErr != nil {
				return c, apiErr
			}
			c.body, c.recordExec, c.note = out, true, "cmd "+argv
		case "start":
			c.body = []byte{} // start options are legacy host config; never forwarded
		}
		id, apiErr := b.owned(ctx, g, segs[1])
		if apiErr != nil {
			return c, apiErr
		}
		c.target, c.path = id, version+"/containers/"+id+"/"+action
		return c, nil
	}
	c.op = "containers"
	return c, forbidden
}

// owned resolves a container name or id and checks its grant label. Foreign
// containers look like missing ones.
func (b *Broker) owned(ctx context.Context, g *grant, idOrName string) (string, *apiError) {
	notFound := &apiError{status: http.StatusNotFound, msg: "No such container: " + idOrName}
	if idOrName == "" {
		return "", notFound
	}
	resp, err := b.engineDo(ctx, http.MethodGet, "/containers/"+url.PathEscape(idOrName)+"/json", nil)
	if err != nil {
		return "", &apiError{status: http.StatusBadGateway, msg: "docker engine unavailable"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", notFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", &apiError{status: http.StatusBadGateway, msg: fmt.Sprintf("docker engine status %d", resp.StatusCode)}
	}
	var info struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&info); err != nil || info.ID == "" {
		return "", &apiError{status: http.StatusBadGateway, msg: "unexpected docker engine answer"}
	}
	if info.Config.Labels[LabelGrant] != g.id {
		notFound.reason = "container not labelled with this grant"
		return "", notFound
	}
	return info.ID, nil
}

// rewriteExec rebuilds an exec create body from allowlisted fields and
// returns it with the command line for the audit record.
func rewriteExec(body []byte) ([]byte, string, *apiError) {
	var in struct {
		AttachStdin  bool
		AttachStdout bool
		AttachStderr bool
		DetachKeys   string
		Tty          bool
		ConsoleSize  *[2]uint
		Env          []string
		Cmd          strSlice
		WorkingDir   string
		User         string
		Privileged   bool
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, "", badRequest("exec config is not valid: %v", err)
	}
	if in.Privileged {
		return nil, "", badRequest("privileged exec is not allowed")
	}
	if len(in.Cmd) == 0 {
		return nil, "", badRequest("exec needs a command")
	}
	out := struct {
		AttachStdin  bool
		AttachStdout bool
		AttachStderr bool
		DetachKeys   string `json:",omitempty"`
		Tty          bool
		ConsoleSize  *[2]uint `json:",omitempty"`
		Env          []string `json:",omitempty"`
		Cmd          []string
		WorkingDir   string `json:",omitempty"`
		User         string `json:",omitempty"`
		Privileged   bool
	}{in.AttachStdin, in.AttachStdout, in.AttachStderr, in.DetachKeys, in.Tty, in.ConsoleSize,
		in.Env, in.Cmd, in.WorkingDir, in.User, false}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, "", badRequest("cannot encode exec config")
	}
	argv, _ := json.Marshal([]string(in.Cmd))
	return enc, truncate(string(argv), 512), nil
}

func readBody(r *http.Request) ([]byte, *apiError) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, badRequest("cannot read request body")
	}
	if len(body) > maxBodyBytes {
		return nil, &apiError{status: http.StatusRequestEntityTooLarge, msg: "request body larger than 1 MiB"}
	}
	return body, nil
}

// addLabelFilter adds a label filter to a Docker filters argument, which
// may use the list form {"label":["a"]} or the legacy map form
// {"label":{"a":true}}. Label filters are ANDed by the Engine.
func addLabelFilter(raw, label string) (string, error) {
	out := map[string][]string{}
	if raw != "" {
		var in map[string]any
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return "", err
		}
		for k, v := range in {
			switch x := v.(type) {
			case []any:
				for _, e := range x {
					s, ok := e.(string)
					if !ok {
						return "", fmt.Errorf("filter %q is malformed", k)
					}
					out[k] = append(out[k], s)
				}
			case map[string]any:
				for s, on := range x {
					if b, _ := on.(bool); b {
						out[k] = append(out[k], s)
					}
				}
			default:
				return "", fmt.Errorf("filter %q is malformed", k)
			}
		}
	}
	out["label"] = append(out["label"], label)
	enc, err := json.Marshal(out)
	return string(enc), err
}

func (b *Broker) rewrite(pr *httputil.ProxyRequest) {
	c, _ := pr.In.Context().Value(callKey{}).(*call)
	u := &url.URL{Scheme: "http", Host: engineHost}
	if c != nil {
		u.Path, u.RawQuery = c.path, c.query
	}
	pr.Out.URL = u
	pr.Out.Host = engineHost
	pr.Out.Header.Del("Authorization")
}

func (b *Broker) modifyResponse(resp *http.Response) error {
	c, _ := resp.Request.Context().Value(callKey{}).(*call)
	if c == nil {
		return nil
	}
	c.status = resp.StatusCode
	switch {
	case c.filterImages && resp.StatusCode == http.StatusOK && resp.Request.Method == http.MethodGet:
		raw, err := readAll(resp)
		if err != nil {
			return err
		}
		filtered, err := filterImageList(raw, c.rules)
		if err != nil {
			return err
		}
		setBody(resp, filtered)
	case c.recordExec && resp.StatusCode == http.StatusCreated:
		raw, err := readAll(resp)
		if err != nil {
			return err
		}
		var created struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(raw, &created); err == nil && created.ID != "" {
			b.mu.Lock()
			c.g.execs[created.ID] = true
			b.mu.Unlock()
		}
		setBody(resp, raw)
	}
	return nil
}

func readAll(resp *http.Response) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > maxListBytes {
		return nil, errors.New("engine response too large")
	}
	return raw, nil
}

func setBody(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	resp.Header.Del("Content-Encoding")
}

// filterImageList keeps the images (and their tags) the grant allows.
func filterImageList(raw []byte, rules []imageRule) ([]byte, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	kept := make([]map[string]json.RawMessage, 0, len(items))
	for _, it := range items {
		keep := false
		for _, key := range []string{"RepoTags", "RepoDigests"} {
			var refs []string
			if v, ok := it[key]; ok {
				_ = json.Unmarshal(v, &refs)
			}
			allowed := make([]string, 0, len(refs))
			for _, s := range refs {
				if ref, _, ok := parseRef(s, false); ok && imageAllowed(rules, ref) {
					allowed = append(allowed, s)
				}
			}
			if _, ok := it[key]; ok {
				enc, _ := json.Marshal(allowed)
				it[key] = enc
			}
			keep = keep || len(allowed) > 0
		}
		if keep {
			kept = append(kept, it)
		}
	}
	return json.Marshal(kept)
}

func (b *Broker) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if c, _ := r.Context().Value(callKey{}).(*call); c != nil {
		c.err = err
		c.status = http.StatusBadGateway
	}
	msg := "docker engine unavailable"
	status := http.StatusBadGateway
	if errors.Is(err, context.Canceled) {
		msg, status = "request cancelled by the broker", http.StatusServiceUnavailable
	}
	writeError(w, status, msg)
}

func (b *Broker) deny(w http.ResponseWriter, rec broker.Record, e *apiError) {
	rec.Allowed = false
	rec.Reason = e.msg
	if e.reason != "" {
		rec.Reason = e.reason
	}
	b.log.Info("docker request denied", "grant", rec.GrantID, "op", rec.Op, "target", rec.Target,
		"status", e.status, "reason", rec.Reason)
	b.emit(rec)
	writeError(w, e.status, e.msg)
}

// writeError answers in the Engine's error shape, which Docker clients print.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "mycel: " + msg})
}

// statusWriter keeps Flush and Hijack reachable through Unwrap.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
