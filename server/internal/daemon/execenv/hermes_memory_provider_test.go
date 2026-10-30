package execenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parseHermesConfigDoc parses a config.yaml body into a document node, the way
// writeDerivedHermesConfig does.
func parseHermesConfigDoc(t *testing.T, body string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return &doc
}

// writeDocToTemp marshals a parsed config document into a temp file and returns
// its path, so assertions can read it back through the same helper the other
// overlay tests use.
func writeDocToTemp(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := marshalYAMLToFile(doc, path); err != nil {
		t.Fatalf("write derived config: %v", err)
	}
	return path
}

// TestHermesMemoryProviderPolicy pins the opt-in contract: the shared config's
// memory.provider survives only when the daemon opted into that same provider.
// Every other combination keeps the isolation default (blank provider).
func TestHermesMemoryProviderPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		config   string
		optIn    string
		want     string
		wantKept bool
	}{
		{
			name:   "no opt-in keeps isolation",
			config: "memory:\n  provider: hindsight\n",
			optIn:  "",
			want:   "",
		},
		{
			name:     "opt-in matching the shared provider is kept",
			config:   "memory:\n  provider: hindsight\n",
			optIn:    "hindsight",
			want:     "hindsight",
			wantKept: true,
		},
		{
			name:   "opt-in for a different provider still blanks",
			config: "memory:\n  provider: supermemory\n",
			optIn:  "hindsight",
			want:   "",
		},
		{
			name:   "opt-in with no configured provider blanks",
			config: "model: hermes-4\n",
			optIn:  "hindsight",
			want:   "",
		},
		{
			name:   "blank configured provider cannot be opted into",
			config: "memory:\n  provider: \"\"\n",
			optIn:  "hindsight",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := parseHermesConfigDoc(t, tc.config)
			hermesMemoryProviderPolicy(doc, tc.optIn)
			got, present := hermesMemoryProvider(t, writeDocToTemp(t, doc))
			if !present {
				t.Fatal("memory.provider should always be present in the derived config")
			}
			if got != tc.want {
				t.Errorf("memory.provider = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestHermesMemoryProviderOptInParsing covers the env gate itself: only a
// provider-shaped name counts as an opt-in, so a stray value cannot enable
// anything.
func TestHermesMemoryProviderOptInParsing(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"   ":                     "",
		"hindsight":               "hindsight",
		"  hindsight  ":           "hindsight",
		"Hindsight":               "",
		"hindsight; memory":       "",
		"../../etc/passwd":        "",
		"provider: hindsight\n":   "",
		"hindsight-memory_plugin": "hindsight-memory_plugin",
	}
	for raw, want := range cases {
		t.Setenv(MulticaHermesMemoryProviderEnv, raw)
		if got := hermesMemoryProviderOptIn(); got != want {
			t.Errorf("hermesMemoryProviderOptIn(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestHermesMemoryProviderOptInThroughOverlay runs the real prepare path with
// the env opt-in set, so the derived config — not just the policy helper — is
// what the assertions read. It also pins that unrelated memory settings survive.
func TestHermesMemoryProviderOptInThroughOverlay(t *testing.T) {
	sharedHome := t.TempDir()
	mustWrite(t, filepath.Join(sharedHome, "config.yaml"),
		"memory:\n  provider: hindsight\n  memory_enabled: false\n")

	t.Setenv(MulticaHermesMemoryProviderEnv, "hindsight")

	hermesHome := filepath.Join(t.TempDir(), "hermes-home")
	skills := []SkillContextForEnv{{Name: "Review Helper", Content: "x"}}
	if _, err := prepareHermesHome(hermesHome, sharedHome, false, skills, nil, "", "", testLogger()); err != nil {
		t.Fatalf("prepareHermesHome failed: %v", err)
	}

	configPath := filepath.Join(hermesHome, "config.yaml")
	got, ok := hermesMemoryProvider(t, configPath)
	if !ok {
		t.Fatal("memory.provider should be present")
	}
	if got != "hindsight" {
		t.Errorf("memory.provider = %q, want \"hindsight\"", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read derived config: %v", err)
	}
	if !strings.Contains(string(data), "memory_enabled: false") {
		t.Error("unrelated memory settings should be preserved")
	}
}
