package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

func TestConfiguredModelsAdvertiseImageInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err := provider.Save(provider.Provider{
		ID: "vision", Name: "Vision", Chat: "https://example.test/v1", Key: "key",
		Models: []string{"image", "text", "unknown"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("vision", "https://example.test/v1", []catalog.Model{
		{ID: "image", Images: true, ImageInput: imageInputBool(true)},
		{ID: "text", ImageInput: imageInputBool(false)},
		{ID: "unknown"},
	}); err != nil {
		t.Fatal(err)
	}
	read := func(path string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(b, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	if err := opencode(home, filepath.Join(home, ".config")).Field("model").Set("magpie/vision/image"); err != nil {
		t.Fatal(err)
	}
	oc := read(filepath.Join(home, ".config", "opencode", "opencode.json"))
	ocModels := oc["provider"].(map[string]any)["magpie"].(map[string]any)["models"].(map[string]any)
	image := ocModels["vision/image"].(map[string]any)
	if image["attachment"] != true {
		t.Fatalf("OpenCode image attachment: %v", image)
	}
	modalities := image["modalities"].(map[string]any)
	if !reflect.DeepEqual(modalities["input"], []any{"text", "image"}) || !reflect.DeepEqual(modalities["output"], []any{"text"}) {
		t.Fatalf("OpenCode image modalities: %v", modalities)
	}
	for _, id := range []string{"vision/text", "vision/unknown"} {
		entry := ocModels[id].(map[string]any)
		if entry["attachment"] != nil || entry["modalities"] != nil {
			t.Fatalf("OpenCode %s incorrectly advertises image input: %v", id, entry)
		}
	}

	if err := pi(home).Field("model").Set("magpie/vision/image"); err != nil {
		t.Fatal(err)
	}
	pm := read(filepath.Join(home, ".pi", "agent", "models.json"))
	piModels := pm["providers"].(map[string]any)["magpie"].(map[string]any)["models"].([]any)
	if len(piModels) != 3 {
		t.Fatalf("Pi models: %v", piModels)
	}
	for _, raw := range piModels {
		entry := raw.(map[string]any)
		switch entry["id"] {
		case "vision/image":
			if !reflect.DeepEqual(entry["input"], []any{"text", "image"}) {
				t.Fatalf("Pi image input: %v", entry)
			}
		case "vision/text", "vision/unknown":
			if entry["input"] != nil {
				t.Fatalf("Pi %s incorrectly advertises image input: %v", entry["id"], entry)
			}
		default:
			t.Fatalf("unexpected Pi model: %v", entry)
		}
	}
}

func imageInputBool(v bool) *bool { return &v }
