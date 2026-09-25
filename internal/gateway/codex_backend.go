package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// CodexPath is where Codex's built-in OpenAI provider reaches magpie, as its
// `openai_base_url`. Codex keeps its own sign-in and sends what it would send
// the ChatGPT backend: a request for one of its own models goes on there as
// it came, one for a magpie model (a catalog id, provider/model) is served
// like any other, and the model list is OpenAI's with magpie's added. Ending
// in /backend-api/codex keeps Codex treating it as that backend.
const CodexPath = "/backend-api/codex"

// codexAPIBase is where a Codex signed in with an API key sends its requests;
// its model list still comes from the ChatGPT backend. A var so tests can
// point it elsewhere.
var codexAPIBase = "https://api.openai.com/v1"

// magpieCompaction marks a compaction item magpie made: its summary, which
// only magpie reads back.
const magpieCompaction = "magpie1:"

// codexCompactPrompt and codexSummaryPrefix are Codex's own (Apache-2.0,
// openai/codex, prompts/templates/compact).
const codexCompactPrompt = `You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task.

Include:
- Current progress and key decisions made
- Important context, constraints, or user preferences
- What remains to be done (clear next steps)
- Any critical data, examples, or references needed to continue

Be concise, structured, and focused on helping the next LLM seamlessly continue the work.`

const codexSummaryPrefix = `Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:`

func (s *Server) codexBackend(w http.ResponseWriter, r *http.Request) {
	// Responses over a WebSocket: 426 sends Codex to plain HTTP at once
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "magpie speaks HTTP", http.StatusUpgradeRequired)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, CodexPath)
	body, err := codexBody(r)
	if err != nil {
		writeError(w, provider.Responses, 400, err.Error())
		return
	}
	switch {
	case r.Method == http.MethodGet && rest == "/models":
		s.codexModels(w, r)
		return
	case r.Method == http.MethodPost && rest == "/responses":
		if model := modelOf(body); isCatalogID(model) {
			body, compact := codexInput(body, true)
			if compact {
				s.codexCompact(w, r, body)
				return
			}
			s.serve(w, r, provider.Responses, body)
			return
		}
		body, _ = codexInput(body, false)
	}
	s.codexUpstream(w, r, rest, body)
}

// codexBody reads a request's body as it was before Codex compressed it
// (zstd, for the ChatGPT backend), so it can be read and passed on plain.
func codexBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "zstd":
		d, err := zstd.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer d.Close()
		rd = d
	case "gzip":
		g, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		rd = g
	default:
		return nil, fmt.Errorf("magpie can't read a %s body", enc)
	}
	r.Header.Del("Content-Encoding")
	return io.ReadAll(io.LimitReader(rd, 64<<20))
}

// isCatalogID reports whether a model is one of magpie's, by its id.
// Codex's own models are bare slugs; magpie's are provider/model.
func isCatalogID(model string) bool {
	if !strings.Contains(model, "/") {
		return false
	}
	for _, e := range provider.Served() {
		if e.ID == model {
			return true
		}
	}
	return false
}

// codexUpstream relays a request as it came, the sign-in included, to where
// Codex would have sent it.
func (s *Server) codexUpstream(w http.ResponseWriter, r *http.Request, rest string, body []byte) {
	start := time.Now()
	base := provider.CodexBase
	if apiKey(r.Header) && rest != "/models" {
		base = codexAPIBase
	}
	u := base + rest
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, bytes.NewReader(body))
	if err != nil {
		writeError(w, provider.Responses, 502, err.Error())
		return
	}
	copyHeaders(req.Header, r.Header)
	// left to the transport, the reply comes back plain for the usage in it
	req.Header.Del("Accept-Encoding")
	res, err := s.client.Do(req)
	if err != nil {
		writeError(w, provider.Responses, 502, "OpenAI: "+err.Error())
		return
	}
	defer res.Body.Close()
	for k, vs := range res.Header {
		if !hopHeader(k) {
			w.Header()[k] = vs
		}
	}
	modelsEtag(w.Header())
	w.WriteHeader(res.StatusCode)
	var sniff *usageSniffer
	if rest == "/responses" {
		ct := res.Header.Get("Content-Type")
		if ct == "" && streamOf(body) { // the ChatGPT backend streams without saying so
			ct = "text/event-stream"
		}
		sniff = newSniffer(provider.Responses, ct)
	}
	f, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if sniff != nil {
				sniff.write(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if sniff == nil {
		return
	}
	call := Call{Time: start, From: provider.Responses, To: provider.Responses, Model: modelOf(body),
		Provider: "openai", Agent: usage.AgentOf(r.Header.Get("User-Agent")), Status: res.StatusCode,
		Millis: time.Since(start).Milliseconds()}
	var uu Usage
	uu.add(sniff.usage())
	call.Usage = uu
	if res.StatusCode >= 400 {
		call.Error = res.Status
	}
	s.record(call)
	usage.Append(usage.Record{Time: start, Agent: call.Agent, Provider: call.Provider, Model: call.Model,
		Input: uu.Input, Output: uu.Output, CacheRead: uu.CacheRead, CacheWrite: uu.CacheWrite,
		Reasoning: uu.Reasoning, Millis: call.Millis, Status: call.Status})
}

// apiKey reports whether Codex signed in with an API key rather than a
// ChatGPT account.
func apiKey(h http.Header) bool {
	return strings.HasPrefix(strings.TrimPrefix(h.Get("Authorization"), "Bearer "), "sk-")
}

func hopHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Upgrade", "Te", "Trailer", "Content-Length":
		return true
	}
	return false
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if !hopHeader(k) && http.CanonicalHeaderKey(k) != "Host" {
			dst[k] = vs
		}
	}
}

// codexModels is the ChatGPT backend's model list for this sign-in, with
// magpie's models after it. Should the backend not answer, Codex's last
// list of its own stands in.
func (s *Server) codexModels(w http.ResponseWriter, r *http.Request) {
	var own []any
	etag := ""
	u := provider.CodexBase + "/models"
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	if req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil); err == nil {
		copyHeaders(req.Header, r.Header)
		req.Header.Del("Accept-Encoding")
		if res, err := s.client.Do(req); err == nil {
			var list struct {
				Models []any `json:"models"`
			}
			b, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
			res.Body.Close()
			if res.StatusCode < 300 && json.Unmarshal(b, &list) == nil {
				own, etag = list.Models, res.Header.Get("ETag")
			} else if res.StatusCode == 401 || res.StatusCode == 403 {
				// a sign-in to renew is Codex's to see
				w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
				w.WriteHeader(res.StatusCode)
				w.Write(b)
				return
			}
		}
	}
	if own == nil {
		for _, e := range codexcat.CacheEntries() {
			own = append(own, e)
		}
	}
	ms := provider.CodexListed()
	// the list is the backend's and magpie's, and so is its ETag
	w.Header().Set("ETag", codexcat.WithTag(etag, codexcat.Tag(ms)))
	writeJSON(w, 200, map[string]any{"models": append(own, codexcat.Entries(ms, len(own)+100)...)})
}

// modelsEtag is the X-Models-Etag of a backend reply as Codex should read
// it: with magpie's models in it, as the list's own ETag has them, so a
// change to either has Codex ask for the list again.
func modelsEtag(h http.Header) {
	if v := h.Get("X-Models-Etag"); v != "" {
		h.Set("X-Models-Etag", codexcat.WithTag(v, codexcat.Tag(provider.CodexListed())))
	}
}

// codexInput restores summaries magpie made. For a magpie model, it also
// replaces Codex's compaction trigger with a request to summarise the input.
// OpenAI's own models keep the trigger for their backend to handle.
func codexInput(body []byte, magpieModel bool) (_ []byte, compact bool) {
	var q map[string]json.RawMessage
	if json.Unmarshal(body, &q) != nil {
		return body, false
	}
	var items []map[string]any
	if json.Unmarshal(q["input"], &items) != nil {
		return body, false
	}
	changed := false
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		switch it["type"] {
		case "compaction", "compaction_summary":
			enc, _ := it["encrypted_content"].(string)
			sum, ok := strings.CutPrefix(enc, magpieCompaction)
			if !ok {
				out = append(out, it) // OpenAI's own, for OpenAI
				continue
			}
			changed = true
			if b, err := base64.StdEncoding.DecodeString(sum); err == nil {
				out = append(out, userMessage(codexSummaryPrefix+"\n"+string(b)))
			}
		case "compaction_trigger":
			if !magpieModel {
				out = append(out, it)
				continue
			}
			changed, compact = true, true
			out = append(out, userMessage(codexCompactPrompt))
		default:
			out = append(out, it)
		}
	}
	if !changed {
		return body, false
	}
	b, _ := json.Marshal(out)
	q["input"] = b
	if compact {
		// the summary is text; a tool call would be no summary
		delete(q, "tools")
		delete(q, "tool_choice")
		delete(q, "parallel_tool_calls")
		q["stream"] = json.RawMessage("false")
	}
	nb, err := json.Marshal(q)
	if err != nil {
		return body, false
	}
	return nb, compact
}

func userMessage(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": text}}}
}

// codexCompact compacts a conversation held by a magpie model: the model
// summarises it, and the summary goes back to Codex as the compaction item
// the backend would have made, marked as magpie's so magpie reads it back
// in later requests.
func (s *Server) codexCompact(w http.ResponseWriter, r *http.Request, body []byte) {
	rec := &recorder{header: http.Header{}, status: 200}
	s.serve(rec, r, provider.Responses, body)
	if rec.status >= 400 {
		for k, vs := range rec.header {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.status)
		w.Write(rec.body.Bytes())
		return
	}
	var res struct {
		ID     string `json:"id"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &res); err != nil {
		writeError(w, provider.Responses, 502, "compaction: "+err.Error())
		return
	}
	var sum strings.Builder
	for _, o := range res.Output {
		if o.Type != "message" {
			continue
		}
		for _, c := range o.Content {
			sum.WriteString(c.Text)
		}
	}
	if strings.TrimSpace(sum.String()) == "" {
		writeError(w, provider.Responses, 502, "compaction: the model wrote no summary")
		return
	}
	id := res.ID
	if id == "" {
		id = fmt.Sprintf("resp_magpie_%d", time.Now().UnixNano())
	}
	item := map[string]any{"type": "compaction", "id": "cmp_" + strings.TrimPrefix(id, "resp_"),
		"encrypted_content": magpieCompaction + base64.StdEncoding.EncodeToString([]byte(sum.String()))}
	done := map[string]any{"id": id, "object": "response", "status": "completed", "output": []any{item}}
	if len(res.Usage) > 0 {
		done["usage"] = res.Usage
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	for _, ev := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": item},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": done},
	} {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// recorder keeps a reply for a second look.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
