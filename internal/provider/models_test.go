package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func TestExplicitTextOnlyBeatsCrossProviderImageGuess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{
		"a":{"models":{"shared":{"id":"shared","modalities":{"input":["text","image"],"output":["text"]}}}},
		"b":{"models":{"shared":{"id":"shared","modalities":{"input":["text","image"],"output":["text"]}}}}
	}`
	if err := os.WriteFile(catalog.CachePath(), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	if !catalog.SeesImages("shared") {
		t.Fatal("test catalog did not classify shared model as vision")
	}
	if err := Save(Provider{ID: "probe", Chat: "https://example.test/v1", Key: "key", Models: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}
	no := false
	if err := catalog.SaveLive("probe", "https://example.test/v1", []catalog.Model{{ID: "shared", ImageInput: &no}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range Catalog() {
		if e.ID == "probe/shared" {
			if e.Images || e.ImageInput == nil || *e.ImageInput {
				t.Fatalf("explicit text-only model advertised images: %+v", e)
			}
			return
		}
	}
	t.Fatal("probe/shared missing from catalog")
}

func TestFetchedKeysShareImageCapability(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := `["text","image"]`
		if r.Header.Get("Authorization") == "Bearer text-key" {
			input = `["text"]`
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"shared","modalities":{"input":` + input + `}}]}`))
	}))
	defer server.Close()
	p := Provider{ID: "relay", Chat: server.URL, Key: "vision-key", Keys: []KeyAccount{{Key: "text-key"}}}
	models, err := p.Fetch(context.Background())
	if err != nil || len(models) != 1 || models[0].ImageInput == nil || *models[0].ImageInput || models[0].Images {
		t.Fatalf("shared image capability: %+v, %v", models, err)
	}
}

func TestFetchedKeysKeepUnknownImageCapability(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := `,"modalities":{"input":["text","image"]}`
		if r.Header.Get("Authorization") == "Bearer unknown-key" {
			input = ""
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"shared"` + input + `}]}`))
	}))
	defer server.Close()
	p := Provider{ID: "relay", Chat: server.URL, Key: "vision-key", Keys: []KeyAccount{{Key: "unknown-key"}}}
	models, err := p.Fetch(context.Background())
	if err != nil || len(models) != 1 || models[0].ImageInput != nil {
		t.Fatalf("confirmed and unknown image capability: %+v, %v", models, err)
	}
}

func TestFetchedKeysKeepUnknownImageCapabilityFromOldCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer failed-key" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"shared","modalities":{"input":["text","image"]}}]}`))
	}))
	defer server.Close()
	if err := catalog.SaveLive("relay", server.URL, []catalog.Model{{ID: "shared", Keys: []string{keyID("failed-key")}}}); err != nil {
		t.Fatal(err)
	}
	p := Provider{ID: "relay", Chat: server.URL, Key: "vision-key", Keys: []KeyAccount{{Key: "failed-key"}}}
	models, err := p.Fetch(context.Background())
	if err != nil || len(models) != 1 || models[0].ImageInput != nil {
		t.Fatalf("fresh capability and old cache without ImageInput: %+v, %v", models, err)
	}
}

func TestRejectsTemperatureFromFetchedList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", home)

	no, yes := false, true
	if err := catalog.SaveLive("p", "http://x", []catalog.Model{
		{ID: "strict", Name: "Strict", Temperature: &no},
		{ID: "plain", Name: "Plain", Temperature: &yes},
		{ID: "silent", Name: "Silent"},
	}); err != nil {
		t.Fatal(err)
	}
	p := Provider{ID: "p"}
	for model, want := range map[string]bool{"strict": true, "plain": false, "silent": false, "unknown": false} {
		if got := p.RejectsTemperature(model); got != want {
			t.Errorf("RejectsTemperature(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestIsOpenCode(t *testing.T) {
	for base, want := range map[string]bool{
		"https://opencode.ai/zen/go/v1": true,
		"https://api.opencode.ai/v1":    true,
		"https://notopencode.ai/v1":     false,
		"https://api.deepseek.com/v1":   false,
	} {
		if got := (Provider{Chat: base}).IsOpenCode(); got != want {
			t.Errorf("%s: %v", base, got)
		}
	}
}
