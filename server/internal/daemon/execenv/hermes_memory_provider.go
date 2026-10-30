package execenv

import (
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// MulticaHermesMemoryProviderEnv opts a daemon into keeping the shared Hermes
// home's external memory provider (Hindsight, Supermemory, ...) in the
// per-task derived config. Unset — the default — keeps the isolation
// behaviour: `memory.provider` is blanked so a managed task never reaches the
// user's memory bank.
//
// The value must name a provider (the same string the shared config.yaml
// selects). The opt-in is deliberately a double gate: a task keeps the
// provider only when the daemon names it AND the user's shared config already
// selects that exact provider, so a task can never acquire a backend the user
// did not configure, and a typo degrades to the isolated default rather than
// enabling something else.
//
// Everything else the provider needs is already reachable from the overlay:
// $HERMES_HOME/hindsight/ (or another provider's config dir) and the plugin
// checkout are mirrored from the shared home, and the derived .env keeps the
// shared credentials, so only the provider switch itself has to survive.
const MulticaHermesMemoryProviderEnv = "MULTICA_HERMES_MEMORY_PROVIDER"

// hermesMemoryProviderNameRe matches the provider identifiers Hermes accepts
// (plugin names: lowercase, digits, `-` and `_`). Anything else — including
// whitespace, a path, or a YAML fragment — is treated as unset.
var hermesMemoryProviderNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// hermesMemoryProviderOptIn returns the provider the daemon opted into, or ""
// when the opt-in is unset or malformed.
func hermesMemoryProviderOptIn() string {
	raw := strings.TrimSpace(os.Getenv(MulticaHermesMemoryProviderEnv))
	if !hermesMemoryProviderNameRe.MatchString(raw) {
		return ""
	}
	return raw
}

// hermesConfigMemoryProvider returns memory.provider from a parsed config
// document, or "" when absent, blank, or not a scalar.
func hermesConfigMemoryProvider(doc *yaml.Node) string {
	top := yamlDocumentRoot(doc)
	if top == nil {
		return ""
	}
	memory := yamlMapValue(top, "memory")
	if memory == nil || memory.Kind != yaml.MappingNode {
		return ""
	}
	provider := yamlMapValue(memory, "provider")
	if provider == nil || provider.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(provider.Value)
}

// applyHermesMemoryProviderPolicy decides the derived config's memory.provider.
// The shared config's provider survives only when the daemon opted into exactly
// that provider; every other case is the isolation default.
func applyHermesMemoryProviderPolicy(doc *yaml.Node) {
	hermesMemoryProviderPolicy(doc, hermesMemoryProviderOptIn())
}

// hermesMemoryProviderPolicy is the pure form of the decision, so tests can pin
// the opt-in without touching the process environment.
func hermesMemoryProviderPolicy(doc *yaml.Node, optIn string) {
	if optIn != "" && hermesConfigMemoryProvider(doc) == optIn {
		return
	}
	disableHermesMemoryProvider(doc)
}
