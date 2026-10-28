package ollama

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	maxFilterBytes = 16 << 20 // model lists and buffered non-streaming responses
	maxLineBytes   = 1 << 20  // longest NDJSON/SSE line that is still parsed
)

// normModel normalises a model name so "llama3", "library/llama3" and
// "llama3:latest" compare equal. It returns "" for names that cannot be a
// model.
func normModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	s = strings.TrimPrefix(s, "registry.ollama.ai/")
	s = strings.TrimPrefix(s, "library/")
	last := s[strings.LastIndex(s, "/")+1:]
	if last == "" {
		return ""
	}
	if !strings.Contains(last, ":") {
		s += ":latest"
	}
	return s
}

// filterModelList keeps only allowed models in an /api/tags or /v1/models
// response, preserving every other field.
func filterModelList(raw []byte, kind filterKind, allowed map[string]bool) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	key := "models"
	if kind == filterModels {
		key = "data"
	}
	var items []json.RawMessage
	if v, ok := top[key]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &items); err != nil {
			return nil, err
		}
	}
	kept := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var m struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			ID    string `json:"id"`
		}
		if json.Unmarshal(it, &m) != nil {
			continue
		}
		for _, s := range []string{m.Name, m.Model, m.ID} {
			if n := normModel(s); n != "" && allowed[n] {
				kept = append(kept, it)
				break
			}
		}
	}
	enc, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	top[key] = enc
	return json.Marshal(top)
}

// reqFields are the request fields the broker reads. The stdlib decoder
// matches keys case-insensitively; checkKeys has already refused bodies in
// which these keys are duplicated or spelt differently, so every upstream
// (Go or not) reads the same values.
type reqFields struct {
	Model   string `json:"model"`
	Name    string `json:"name"`
	Stream  *bool  `json:"stream"`
	Options struct {
		NumPredict int64 `json:"num_predict"`
	} `json:"options"`
	MaxTokens           int64 `json:"max_tokens"`
	MaxCompletionTokens int64 `json:"max_completion_tokens"`
}

// wanted is the generation limit the client asked for; 0 = none.
func (f reqFields) wanted(rt route) int64 {
	if !rt.openai {
		return max(f.Options.NumPredict, 0)
	}
	w := f.MaxTokens
	if c := f.MaxCompletionTokens; c > 0 && (w <= 0 || c < w) {
		w = c
	}
	return max(w, 0)
}

// watchedKeys are the top-level keys whose spelling must be canonical and
// unique; watchedOptions the same inside "options".
var (
	watchedKeys = map[string]string{
		"model": "model", "name": "name", "stream": "stream", "stream_options": "stream_options",
		"options": "options", "max_tokens": "max_tokens", "max_completion_tokens": "max_completion_tokens",
	}
	watchedOptions = map[string]string{"num_predict": "num_predict"}
)

// checkKeys refuses a body whose watched keys are duplicated or not in
// canonical case.
func checkKeys(body []byte) error {
	options, err := scanKeys(body, watchedKeys, "options")
	if err != nil {
		return err
	}
	if len(options) > 0 && options[0] == '{' {
		if _, err := scanKeys(options, watchedOptions, ""); err != nil {
			return err
		}
	}
	return nil
}

// scanKeys walks one JSON object's keys and returns the raw value of key
// nested (if any).
func scanKeys(body []byte, watched map[string]string, nested string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("request body is not a JSON object")
	}
	seen := map[string]bool{}
	var nestedRaw json.RawMessage
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("request body is not valid JSON")
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errors.New("request body is not valid JSON")
		}
		canon, ok := watched[strings.ToLower(key)]
		if !ok {
			continue
		}
		if key != canon {
			return nil, fmt.Errorf("key %q must be spelt %q", key, canon)
		}
		if seen[canon] {
			return nil, fmt.Errorf("duplicate key %q", canon)
		}
		seen[canon] = true
		if canon == nested {
			nestedRaw = raw
		}
	}
	return nestedRaw, nil
}

// rewriteBody clamps generation to limit (options.num_predict for native
// requests, max_tokens for OpenAI-compatible ones) and, for streamed OpenAI
// completions, asks for a final usage chunk so tokens can be counted. The
// body is returned unchanged when it cannot be rewritten.
func rewriteBody(body []byte, rt route, limit int64, includeUsage bool) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return body
	}
	if limit > 0 {
		if rt.openai {
			m["max_tokens"] = limit
			if _, ok := m["max_completion_tokens"]; ok {
				m["max_completion_tokens"] = limit
			}
		} else {
			opts, _ := m["options"].(map[string]any)
			if opts == nil {
				opts = map[string]any{}
			}
			opts["num_predict"] = limit
			m["options"] = opts
		}
	}
	if includeUsage {
		switch opts := m["stream_options"].(type) {
		case nil:
			m["stream_options"] = map[string]any{"include_usage": true}
		case map[string]any:
			if _, set := opts["include_usage"]; !set {
				opts["include_usage"] = true
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// counter extracts generated-token counts from a response body as it
// streams past: Ollama's eval_count (final NDJSON object or the single
// non-streaming object) and OpenAI's usage.completion_tokens. Until a final
// count arrives, every streamed chunk counts as one token, so a stream cut
// before its end is still charged. A body too large to parse is charged at
// four bytes per token.
type counter struct {
	limit    int64 // cut the stream once more than this many tokens were generated; 0 = no limit
	lines    bool  // NDJSON or SSE: parse line by line
	sse      bool
	buf      []byte
	skipping bool // inside an over-long line, or the buffered body overflowed
	bytes    int64
	gen      int64
	prompt   int64
	final    bool // an authoritative count arrived
	chunks   int64
	done     bool
	cut      bool
}

func (c *counter) setMode(contentType string) {
	ct := strings.ToLower(contentType)
	c.sse = strings.Contains(ct, "text/event-stream")
	c.lines = c.sse || strings.Contains(ct, "ndjson")
}

// running is the best count so far.
func (c *counter) running() int64 {
	if c.final {
		return c.gen
	}
	return c.chunks
}

func (c *counter) write(p []byte) {
	c.bytes += int64(len(p))
	if !c.lines {
		if !c.skipping && len(c.buf)+len(p) <= maxFilterBytes {
			c.buf = append(c.buf, p...)
		} else {
			c.buf, c.skipping = nil, true
		}
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !c.skipping {
				if len(c.buf)+len(p) > maxLineBytes {
					c.buf, c.skipping = c.buf[:0], true
				} else {
					c.buf = append(c.buf, p...)
				}
			}
			return
		}
		if !c.skipping {
			c.buf = append(c.buf, p[:i]...)
			c.line(c.buf)
		}
		c.buf, c.skipping = c.buf[:0], false
		p = p[i+1:]
	}
}

func (c *counter) finish() {
	if c.done {
		return
	}
	c.done = true
	switch {
	case !c.lines && c.skipping:
		c.gen, c.final = c.bytes/4, true
	case !c.skipping && len(c.buf) > 0:
		c.line(c.buf)
	}
	c.buf = nil
	if !c.final {
		c.gen = c.chunks
	}
}

func (c *counter) line(l []byte) {
	l = bytes.TrimSpace(l)
	if c.sse {
		if !bytes.HasPrefix(l, []byte("data:")) {
			return
		}
		l = bytes.TrimSpace(l[len("data:"):])
	}
	if len(l) == 0 || l[0] != '{' {
		return
	}
	var f struct {
		EvalCount       *int64 `json:"eval_count"`
		PromptEvalCount int64  `json:"prompt_eval_count"`
		Done            *bool  `json:"done"`
		Usage           *struct {
			CompletionTokens int64 `json:"completion_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
		} `json:"usage"`
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(l, &f) != nil {
		return
	}
	c.prompt += f.PromptEvalCount
	switch {
	case f.EvalCount != nil:
		c.gen += *f.EvalCount
		c.final = true
	case f.Usage != nil:
		c.gen += f.Usage.CompletionTokens
		c.prompt += f.Usage.PromptTokens
		c.final = true
	case len(f.Choices) > 0 || (f.Done != nil && !*f.Done):
		c.chunks++
	}
}

var errBudget = errors.New("token budget of the request used up")

// countingBody feeds the response body to the counter as the proxy copies
// it to the client, and cuts the stream once the request's token budget is
// exceeded.
type countingBody struct {
	io.ReadCloser
	c *counter
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.c.cut {
		return 0, errBudget
	}
	n, err := b.ReadCloser.Read(p)
	if b.c.limit <= 0 || !b.c.lines {
		b.c.write(p[:n])
		return n, err
	}
	// Feed line by line so the line that crosses the budget is withheld.
	off := 0
	for off < n {
		end := n
		if i := bytes.IndexByte(p[off:n], '\n'); i >= 0 {
			end = off + i + 1
		}
		b.c.write(p[off:end])
		if b.c.running() > b.c.limit {
			b.c.cut = true
			return off, errBudget
		}
		off = end
	}
	return n, err
}
