package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/richard-sen27/mycelium/internal/broker"
)

// route describes one allowed path.
type route struct {
	method   string
	model    modelField
	gate     string // "pull", "delete" or ""
	filter   filterKind
	count    bool // the response carries generated-token counts
	openai   bool // OpenAI-compatible completion: stream_options may be injected
	pullLike bool // the model need not be present upstream yet
}

type modelField int

const (
	modelNone     modelField = iota
	modelRequired            // "model"
	modelOrName              // "model" or "name"
)

type filterKind int

const (
	filterNone filterKind = iota
	filterTags
	filterModels
)

var routes = map[string]route{
	"/api/generate":        {method: http.MethodPost, model: modelRequired, count: true},
	"/api/chat":            {method: http.MethodPost, model: modelRequired, count: true},
	"/api/embeddings":      {method: http.MethodPost, model: modelRequired},
	"/api/embed":           {method: http.MethodPost, model: modelRequired},
	"/api/tags":            {method: http.MethodGet, filter: filterTags},
	"/api/show":            {method: http.MethodPost, model: modelOrName},
	"/api/pull":            {method: http.MethodPost, model: modelOrName, gate: "pull", pullLike: true},
	"/api/delete":          {method: http.MethodDelete, model: modelOrName, gate: "delete"},
	"/v1/chat/completions": {method: http.MethodPost, model: modelRequired, count: true, openai: true},
	"/v1/completions":      {method: http.MethodPost, model: modelRequired, count: true, openai: true},
	"/v1/embeddings":       {method: http.MethodPost, model: modelRequired},
	"/v1/models":           {method: http.MethodGet, filter: filterModels},
}

// reqState travels with the outgoing request so ModifyResponse can see the
// grant's terms and the token counter.
type reqState struct {
	terms   terms
	route   route
	counter *counter
	status  int
	err     error
}

type stateKey struct{}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := broker.Record{Broker: broker.KindOllama, Op: truncate(r.URL.Path, maxOpLen)}

	b.mu.Lock()
	frozen, why := b.frozen, b.freezeReason
	b.mu.Unlock()
	if frozen {
		b.deny(w, rec, http.StatusServiceUnavailable, "broker frozen: "+why)
		return
	}

	g := b.lookup(credential(r))
	if g == nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mycel"`)
		b.mu.Lock()
		sampled := b.unauth.allow(b.now(), unauthAuditRate, unauthAuditBurst)
		b.mu.Unlock()
		if sampled {
			b.deny(w, rec, http.StatusUnauthorized, "missing or unknown grant credential")
		} else {
			writeError(w, http.StatusUnauthorized, "missing or unknown grant credential")
		}
		return
	}
	now := b.now()
	b.mu.Lock()
	rec.GrantID, rec.Subject, rec.SandboxID = g.id, g.subject, g.sandboxID
	t, expires, used := g.terms, g.expires, g.used+g.reserved
	b.mu.Unlock()

	if b.ceiling != nil {
		if !b.ceiling.Offers(broker.KindOllama) {
			b.deny(w, rec, http.StatusServiceUnavailable, "model sharing is switched off on this node")
			return
		}
		if !b.ceiling.Active() {
			b.deny(w, rec, http.StatusServiceUnavailable, "sharing is not active on this node right now")
			return
		}
	}
	if !expires.IsZero() && !now.Before(expires) {
		b.deny(w, rec, http.StatusForbidden, "grant expired")
		return
	}
	rt, ok := routes[r.URL.Path]
	if !ok {
		b.deny(w, rec, http.StatusForbidden, "path not allowed")
		return
	}
	if (rt.gate == "pull" && !t.allowPull) || (rt.gate == "delete" && !t.allowDelete) {
		b.deny(w, rec, http.StatusForbidden, rt.gate+" not allowed by the grant")
		return
	}
	if r.Method != rt.method && (rt.method != http.MethodGet || r.Method != http.MethodHead) {
		w.Header().Set("Allow", rt.method)
		b.deny(w, rec, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	b.mu.Lock()
	allowed := g.bucket.allow(now, float64(t.rpm)/60, float64(t.rpm))
	b.mu.Unlock()
	if !allowed {
		w.Header().Set("Retry-After", "1")
		b.deny(w, rec, http.StatusTooManyRequests, "rate limit: "+fmt.Sprint(t.rpm)+" requests per minute")
		return
	}
	if rt.count && t.maxTokens > 0 && used >= t.maxTokens {
		b.deny(w, rec, http.StatusForbidden, fmt.Sprintf("token limit of the grant reached (%d of %d)", used, t.maxTokens))
		return
	}

	// A client gets bodyReadTimeout to send its (small) body.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(now.Add(bodyReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		b.deny(w, rec, http.StatusBadRequest, "cannot read request body")
		return
	}
	if len(body) > maxBodyBytes {
		b.deny(w, rec, http.StatusRequestEntityTooLarge, "request body larger than 1 MiB")
		return
	}
	var req reqFields
	if rt.model != modelNone {
		if err := checkKeys(body); err != nil {
			b.deny(w, rec, http.StatusBadRequest, err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			b.deny(w, rec, http.StatusBadRequest, "request body is not a JSON object")
			return
		}
		model := req.Model
		if model == "" && rt.model == modelOrName {
			model = req.Name
		}
		rec.Target = model
		if model == "" {
			b.deny(w, rec, http.StatusBadRequest, "request names no model")
			return
		}
		if ok, reason := b.modelAllowed(r.Context(), t, rt, model); !ok {
			b.deny(w, rec, http.StatusForbidden, reason)
			return
		}
	}

	b.forward(w, r, g, rt, t, body, req, rec)
}

// forward admits the checked request (in-flight cap, token reservation),
// proxies it upstream and settles its accounting, also when the stream is
// cut.
func (b *Broker) forward(w http.ResponseWriter, r *http.Request, g *grant, rt route, t terms, body []byte, req reqFields, rec broker.Record) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	b.mu.Lock()
	if b.grants[g.id] != g || b.frozen {
		b.mu.Unlock()
		b.deny(w, rec, http.StatusServiceUnavailable, "grant no longer active")
		return
	}
	if len(g.inflight) >= maxInflight {
		b.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		b.deny(w, rec, http.StatusTooManyRequests, fmt.Sprintf("at most %d requests in flight per grant", maxInflight))
		return
	}
	var limit int64 // generated tokens this request may use; 0 = unlimited
	if rt.count && t.maxTokens > 0 {
		remaining := t.maxTokens - g.used - g.reserved
		if remaining <= 0 {
			b.mu.Unlock()
			b.deny(w, rec, http.StatusForbidden, fmt.Sprintf("token limit of the grant reached (%d of %d)", g.used+g.reserved, t.maxTokens))
			return
		}
		limit = remaining
		if want := req.wanted(rt); want > 0 && want < limit {
			limit = want
		}
		g.reserved += limit
	}
	id := g.nextReq
	g.nextReq++
	g.inflight[id] = cancel
	g.dReq++
	if rec.Target != "" {
		g.model = rec.Target
	}
	b.mu.Unlock()

	if rt.count && (limit > 0 || (rt.openai && req.Stream != nil && *req.Stream)) {
		body = rewriteBody(body, rt, limit, rt.openai && req.Stream != nil && *req.Stream)
	}
	st := &reqState{terms: t, route: rt}
	if rt.count {
		st.counter = &counter{limit: limit}
	}
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		// The proxy panics with http.ErrAbortHandler when a stream is cut
		// (client gone, Detach, Freeze, token budget); the request is
		// settled and audited either way.
		p := recover()
		if st.counter != nil {
			st.counter.finish()
			rec.Tokens = st.counter.gen
		}
		b.mu.Lock()
		delete(g.inflight, id)
		g.reserved -= limit
		g.used += rec.Tokens
		g.dTok += rec.Tokens
		if b.grants[g.id] != g {
			// Detached while in flight: the pending entry collects the rest.
			b.keepPending(g)
		}
		b.mu.Unlock()

		rec.Allowed = true
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		switch {
		case st.counter != nil && st.counter.cut:
			rec.Reason = "aborted: token budget of the request used up"
		case p != nil:
			rec.Reason = "aborted"
		case st.err != nil:
			rec.Reason = "upstream: " + st.err.Error()
		case status >= 400:
			rec.Reason = fmt.Sprintf("upstream status %d", status)
		}
		b.log.Info("ollama request", "grant", rec.GrantID, "op", rec.Op, "model", rec.Target,
			"status", status, "tokens", rec.Tokens, "reason", rec.Reason)
		b.emit(rec)
		if p != nil {
			panic(p)
		}
	}()

	out := r.WithContext(context.WithValue(ctx, stateKey{}, st))
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.Header = r.Header.Clone()
	out.Header.Set("Content-Length", fmt.Sprint(len(body)))
	if len(body) == 0 {
		out.Body = http.NoBody
		out.Header.Del("Content-Length")
	}
	b.proxy.ServeHTTP(sw, out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (b *Broker) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(b.upstream)
	q := pr.Out.URL.Query()
	if q.Has("token") {
		q.Del("token")
		pr.Out.URL.RawQuery = q.Encode()
	}
	h := pr.Out.Header
	h.Del("Authorization")
	h.Del("Cookie")
	h.Del("Accept-Encoding") // the transport negotiates and decodes, so counters see plain text
	h.Del("X-Forwarded-For")
	h.Del("Forwarded")
}

func (b *Broker) modifyResponse(resp *http.Response) error {
	st, _ := resp.Request.Context().Value(stateKey{}).(*reqState)
	if st == nil {
		return nil
	}
	st.status = resp.StatusCode
	if resp.StatusCode == http.StatusOK && resp.Request.Method != http.MethodHead &&
		st.route.filter != filterNone && len(st.terms.models) > 0 {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFilterBytes+1))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > maxFilterBytes {
			return errors.New("model list too large")
		}
		filtered, err := filterModelList(raw, st.route.filter, st.terms.models)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(filtered))
		resp.ContentLength = int64(len(filtered))
		resp.Header.Set("Content-Length", fmt.Sprint(len(filtered)))
		resp.Header.Del("Content-Encoding")
		return nil
	}
	if st.counter != nil {
		st.counter.setMode(resp.Header.Get("Content-Type"))
		resp.Body = &countingBody{ReadCloser: resp.Body, c: st.counter}
	}
	return nil
}

func (b *Broker) proxyError(w http.ResponseWriter, r *http.Request, err error) {
	if st, _ := r.Context().Value(stateKey{}).(*reqState); st != nil {
		st.err = err
	}
	status := http.StatusBadGateway
	msg := "model endpoint unavailable"
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status, msg = http.StatusServiceUnavailable, "request cancelled by the broker"
	}
	writeError(w, status, msg)
}

func (b *Broker) deny(w http.ResponseWriter, rec broker.Record, status int, reason string) {
	rec.Allowed = false
	rec.Reason = reason
	b.log.Info("ollama request denied", "grant", rec.GrantID, "op", rec.Op, "model", rec.Target,
		"status", status, "reason", reason)
	b.emit(rec)
	writeError(w, status, reason)
}

func (b *Broker) emit(rec broker.Record) {
	if b.audit != nil {
		b.audit(rec)
	}
}

// writeError answers in the shape both Ollama and OpenAI clients print.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "mycel: " + msg})
}

// modelAllowed checks the requested model against the grant.
func (b *Broker) modelAllowed(ctx context.Context, t terms, rt route, model string) (bool, string) {
	n := normModel(model)
	if n == "" {
		return false, "invalid model name"
	}
	if len(t.models) > 0 {
		if t.models[n] {
			return true, ""
		}
		return false, "model not in the grant's allowlist"
	}
	if rt.pullLike {
		return true, ""
	}
	present, err := b.presentModels(ctx)
	if err != nil {
		return false, "cannot list the models on this node"
	}
	if !present[n] {
		return false, "model not present on this node"
	}
	return true, ""
}

// presentModels returns the upstream's models, refreshed at most once per
// minute.
func (b *Broker) presentModels(ctx context.Context) (map[string]bool, error) {
	b.tagsMu.Lock()
	defer b.tagsMu.Unlock()
	if b.tags != nil && b.now().Sub(b.tagsAt) < tagsTTL {
		return b.tags, nil
	}
	tags, err := b.fetchTags(ctx)
	if err != nil {
		return nil, err
	}
	b.tags, b.tagsAt = tags, b.now()
	return tags, nil
}

func (b *Broker) fetchTags(ctx context.Context) (map[string]bool, error) {
	u := *b.upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/tags"
	ctx, cancel := context.WithTimeout(ctx, tagsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var list struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxFilterBytes)).Decode(&list); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list.Models))
	for _, m := range list.Models {
		for _, s := range []string{m.Name, m.Model} {
			if n := normModel(s); n != "" {
				out[n] = true
			}
		}
	}
	return out, nil
}

// statusWriter records the status code; Unwrap keeps Flush working through
// http.ResponseController.
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

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
