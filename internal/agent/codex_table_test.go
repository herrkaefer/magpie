package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// The magpie table claims OpenAI auth only when Codex is signed in to ChatGPT.
// With the key and no sign-in, Codex opens its sign-in screen and users who
// only run third-party models cannot start it at all.
func TestMagpieTableRequiresOpenAIAuthOnlyWhenSignedIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CODEX_HOME", "")
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek",
		Chat: "https://api.deepseek.com/v1", Key: "k", Models: []string{"deepseek-flash"}}); err != nil {
		t.Fatal(err)
	}

	table := func() string {
		b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	setModel := func() {
		t.Helper()
		if err := codex(home).Field("model").Set("deepseek/deepseek-flash"); err != nil {
			t.Fatal(err)
		}
	}

	setModel()
	if strings.Contains(table(), "requires_openai_auth") {
		t.Fatalf("key written without a ChatGPT sign-in:\n%s", table())
	}

	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{"access_token":"x","id_token":"x.e30.x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setModel()
	if !strings.Contains(table(), "requires_openai_auth = true") {
		t.Fatalf("key missing with a ChatGPT sign-in:\n%s", table())
	}
}
