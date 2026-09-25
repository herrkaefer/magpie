package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/codexcat"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// chatgpt stands in for the ChatGPT backend behind CodexBase.
func chatgpt(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // no sign-ins but the test's
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	return up
}

func codexPost(t *testing.T, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", CodexPath+"/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("chatgpt-account-id", "acct-1")
	New().Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// One of Codex's own models goes on to the ChatGPT backend as it came, the
// sign-in with it; a summary magpie made earlier goes as the text it holds.
func TestCodexOwnModelPassesThrough(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	var got []byte
	var head http.Header
	var path string
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		head, path = r.Header, r.URL.Path
		w.Header()["Content-Type"] = nil // as the ChatGPT backend sends it
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"r1"}}`,
			`data: {"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":9,"output_tokens":2}}}`))
	})
	sum := magpieCompaction + base64.StdEncoding.EncodeToString([]byte("did X, next Y"))
	code, body := codexPost(t, `{"model":"gpt-5.5","stream":true,"input":[
	  {"type":"compaction","encrypted_content":"`+sum+`"},
	  {"type":"compaction","encrypted_content":"openai-own"},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}]}`)
	if code != 200 || !strings.Contains(body, `"input_tokens":9`) {
		t.Fatalf("%d %s", code, body)
	}
	if u := usage.Load(time.Time{}); len(u) != 1 || u[0].Input != 9 || u[0].Output != 2 || u[0].Provider != "openai" {
		t.Errorf("usage %+v", u)
	}
	if path != "/backend-api/codex/responses" || head.Get("Authorization") != "Bearer chatgpt-token" || head.Get("chatgpt-account-id") != "acct-1" {
		t.Errorf("upstream %s %v", path, head)
	}
	var q struct {
		Input []map[string]any `json:"input"`
	}
	json.Unmarshal(got, &q)
	if len(q.Input) != 3 || q.Input[0]["type"] != "message" || !strings.Contains(string(got), "did X, next Y") ||
		q.Input[1]["encrypted_content"] != "openai-own" {
		t.Errorf("input: %s", got)
	}
}

func TestCodexOwnModelCompactionPassesThrough(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	var got [][]byte
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, b)
		if !streamOf(b) {
			http.Error(w, `{"detail":"Stream must be set to true"}`, http.StatusBadRequest)
			return
		}
		w.Header()["Content-Type"] = nil
		io.WriteString(w, sse(
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"compaction","id":"cmp_openai","encrypted_content":"openai-own"}}`,
			`data: {"type":"response.completed","response":{"id":"resp_openai","status":"completed","output":[{"type":"compaction","id":"cmp_openai","encrypted_content":"openai-own"}]}}`))
	})
	original := `{"model":"gpt-6-sol","stream":true,"tools":[{"type":"function","name":"shell"}],"input":[{"type":"message","role":"user","content":"remember this"},{"type":"compaction_trigger"}]}`
	code, body := codexPost(t, original)
	if code != 200 || !strings.Contains(body, `"id":"cmp_openai"`) || len(got) != 1 || string(got[0]) != original {
		t.Fatalf("own compact: %d %s; upstream %q", code, body, got)
	}

	sum := magpieCompaction + base64.StdEncoding.EncodeToString([]byte("prior summary"))
	code, body = codexPost(t, `{"model":"gpt-6-sol","stream":true,"tools":[{"type":"function","name":"shell"}],"input":[{"type":"compaction","encrypted_content":"`+sum+`"},{"type":"compaction_trigger"}]}`)
	if code != 200 || len(got) != 2 {
		t.Fatalf("mixed compact: %d %s; %d upstream requests", code, body, len(got))
	}
	var q struct {
		Stream bool             `json:"stream"`
		Tools  []map[string]any `json:"tools"`
		Input  []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(got[1], &q); err != nil || !q.Stream || len(q.Tools) != 1 || len(q.Input) != 2 ||
		q.Input[0]["type"] != "message" || q.Input[1]["type"] != "compaction_trigger" ||
		!strings.Contains(string(got[1]), "prior summary") || strings.Contains(string(got[1]), codexCompactPrompt) {
		t.Errorf("mixed compact upstream: %s (%v)", got[1], err)
	}
}

// A magpie model is served by magpie, whatever sign-in Codex sent.
func TestCodexMagpieModelServed(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	setup(t, provider.Chat, f)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) { t.Error("sent to ChatGPT") })
	code, body := codexPost(t, `{"model":"fake/m1","stream":true,"input":"hello"}`)
	if code != 200 || !strings.Contains(body, `"delta":"hi"`) {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.Contains(string(f.got), `"model":"m1"`) {
		t.Errorf("upstream got %s", f.got)
	}
}

// Codex compresses what it sends the ChatGPT backend; magpie reads it all
// the same.
func TestCodexReadsCompressedBody(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	setup(t, provider.Chat, f)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) { t.Error("sent to ChatGPT") })
	enc, _ := zstd.NewWriter(nil)
	z := enc.EncodeAll([]byte(`{"model":"fake/m1","stream":true,"input":"hello"}`), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", CodexPath+"/responses", bytes.NewReader(z))
	req.Header.Set("Content-Encoding", "zstd")
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"delta":"hi"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// Compacting a magpie model's conversation: the model summarises, and Codex
// gets the compaction item it asked for, holding the summary.
func TestCodexCompactsMagpieModel(t *testing.T) {
	f := &fake{t: t, reply: sse(
		`data: {"id":"c1","model":"m1","choices":[{"delta":{"content":"SUMM"}}]}`,
		`data: {"id":"c1","choices":[{"delta":{"content":"ARY"},"finish_reason":"stop"}]}`,
		`data: [DONE]`)}
	setup(t, provider.Chat, f)
	code, body := codexPost(t, `{"model":"fake/m1","stream":true,
	  "tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},
	    {"type":"compaction_trigger"}]}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(string(f.got), `"tools"`) || !strings.Contains(string(f.got), "CONTEXT CHECKPOINT COMPACTION") {
		t.Errorf("upstream got %s", f.got)
	}
	var item map[string]any
	completed := false
	for _, e := range events(body) {
		switch e["type"] {
		case "response.output_item.done":
			item = e["item"].(map[string]any)
		case "response.completed":
			completed = true
		}
	}
	enc, _ := item["encrypted_content"].(string)
	b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, magpieCompaction))
	if item["type"] != "compaction" || !strings.HasPrefix(enc, magpieCompaction) || string(b) != "SUMMARY" || !completed {
		t.Errorf("events: %s", body)
	}
	code, _ = codexPost(t, `{"model":"fake/m1","stream":true,"input":[{"type":"compaction","encrypted_content":"`+enc+`"},{"type":"message","role":"user","content":"continue"}]}`)
	if code != 200 || !strings.Contains(string(f.got), "SUMMARY") || !strings.Contains(string(f.got), codexSummaryPrefix) || strings.Contains(string(f.got), magpieCompaction) {
		t.Errorf("compacted conversation was not restored: %d %s", code, f.got)
	}
}

// The model list is the backend's for this sign-in, then magpie's.
func TestCodexModelList(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	var auth string
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, `{"models":[{"slug":"gpt-5.5","priority":1}]}`)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", CodexPath+"/models?client_version=0.155.1", nil)
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	New().Handler().ServeHTTP(rec, req)
	var list struct {
		Models []map[string]any `json:"models"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	var slugs []string
	var fake map[string]any
	for _, m := range list.Models {
		slug, _ := m["slug"].(string)
		slugs = append(slugs, slug)
		if slug == "fake/m1" {
			fake = m
		}
	}
	// Other sign-ins found on the machine (Cursor's, say) may follow. The
	// ETag is the backend's with magpie's list in it.
	tag := codexcat.Tag(provider.CodexListed())
	if rec.Code != 200 || len(slugs) < 2 || slugs[0] != "gpt-5.5" || fake == nil || fake["base_instructions"] == "" ||
		rec.Header().Get("ETag") != `"v1+magpie-`+tag+`"` || auth != "Bearer chatgpt-token" {
		t.Errorf("%d %v %q %q", rec.Code, slugs, rec.Header().Get("ETag"), auth)
	}
	for _, s := range slugs {
		if strings.HasPrefix(s, "codex/") {
			t.Errorf("Codex's own models twice: %v", slugs)
		}
	}
}

// A reply's X-Models-Etag carries magpie's list too: Codex refetches its
// model list on a new one, and only on the backend's it never would when a
// provider was added.
func TestCodexModelsEtagTagged(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Models-Etag", `W/"v1"`)
		io.WriteString(w, sse(
			`data: {"type":"response.created","response":{"id":"r1"}}`,
			`data: {"type":"response.completed","response":{"id":"r1"}}`))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", CodexPath+"/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":[]}`))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	New().Handler().ServeHTTP(rec, req)
	tag := codexcat.Tag(provider.CodexListed())
	if got := rec.Header().Get("X-Models-Etag"); got != `W/"v1+magpie-`+tag+`"` || !codexcat.Tagged(got, tag) {
		t.Errorf("%d X-Models-Etag %q", rec.Code, got)
	}
}

// Responses over a WebSocket are turned away so Codex uses HTTP at once.
func TestCodexWebSocketUpgradeRequired(t *testing.T) {
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) { t.Error("sent to ChatGPT") })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", CodexPath+"/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUpgradeRequired {
		t.Errorf("%d", rec.Code)
	}
}

// A routing group in Codex's model list is named as one, not as its first
// member's provider, which would read as that provider's own model.
func TestCodexModelsNameGroups(t *testing.T) {
	setup(t, provider.Chat, &fake{t: t})
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"models":[]}`) })
	if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"fake/m1"}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest("GET", CodexPath+"/models", nil))
	var list struct {
		Models []struct {
			Slug string `json:"slug"`
			Name string `json:"display_name"`
		} `json:"models"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	names := map[string]string{}
	for _, m := range list.Models {
		names[m.Slug] = m.Name
	}
	if names["group/g"] != "G · routing group" || !strings.HasSuffix(names["fake/m1"], " · Fake") {
		t.Fatalf("%v", names)
	}
}
