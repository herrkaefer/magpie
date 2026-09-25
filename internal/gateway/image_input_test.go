package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

func TestKnownTextOnlyModelRejectsImagesBeforeUpstream(t *testing.T) {
	fresh(t)
	var sent int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "probe", Name: "Probe", Chat: up.URL + "/v1", Key: "key", Models: []string{"text", "vision", "unknown"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("probe", up.URL+"/v1", []catalog.Model{
		{ID: "text", ImageInput: imageInputBool(false)},
		{ID: "vision", Images: true, ImageInput: imageInputBool(true)},
		{ID: "unknown"},
	}); err != nil {
		t.Fatal(err)
	}
	s := New()
	cases := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"probe/text","messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`},
		{"/v1/responses", `{"model":"probe/text","input":[{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`},
		{"/v1/messages", `{"model":"probe/text","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`},
		{"/v1beta/models/probe/text:generateContent", `{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "does not support image input") {
			t.Errorf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	if sent != 0 {
		t.Fatalf("text-only images reached upstream %d times", sent)
	}
	if err := provider.SaveGroup(provider.Group{Name: "Mixed", Members: []string{"probe/text", "probe/vision"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	groupBody := strings.Replace(cases[0].body, "probe/text", "group/mixed", 1)
	group := httptest.NewRecorder()
	s.Handler().ServeHTTP(group, httptest.NewRequest("POST", cases[0].path, strings.NewReader(groupBody)))
	if group.Code != 400 || sent != 0 {
		t.Fatalf("mixed-capability group: %d %s; upstream sent %d", group.Code, group.Body.String(), sent)
	}
	code, body := postAs(t, s, "", `{"model":"probe/text","messages":[{"role":"user","content":"hello"}]}`)
	if code != 200 {
		t.Fatalf("text-only model with text input: %d %s", code, body)
	}
	for _, model := range []string{"vision", "unknown"} {
		body := strings.Replace(cases[0].body, "probe/text", "probe/"+model, 1)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", cases[0].path, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Errorf("%s image: %d %s", model, rec.Code, rec.Body.String())
		}
	}
	if sent != 3 {
		t.Fatalf("text, vision, and unknown requests sent %d times", sent)
	}
}

func imageInputBool(v bool) *bool { return &v }
