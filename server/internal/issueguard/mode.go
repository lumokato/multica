package issueguard

import (
	"os"
	"strings"
)

// Mode says how far the status guard is allowed to go.
//
// The three values are ordered from harmless to decisive, so a deployment can
// move one step at a time and move back by changing one environment variable.
// Nothing else in the process needs to change.
type Mode string

const (
	// ModeOff runs no guard at all: no queries, no logs, behaviour identical
	// to the build before the guard existed.
	ModeOff Mode = "off"
	// ModeShadow judges every transition and logs what it would have
	// rejected, but never blocks. This is the default.
	ModeShadow Mode = "shadow"
	// ModeEnforce rejects transitions with blocking violations. Wired in a
	// later stage; until then it behaves exactly like ModeShadow so that a
	// premature flag cannot deny writes.
	ModeEnforce Mode = "enforce"
)

// ModeEnvVar is the only switch. Reading it per call keeps a restart-free
// rollback honest: flip to "off" and the next write is unguarded.
const ModeEnvVar = "ISSUE_GUARD_MODE"

// DefaultMode is used when the variable is unset.
const DefaultMode = ModeShadow

// ParseMode maps a raw environment value to a Mode. Unset and unrecognised
// values fall back to shadow: observing is the safe direction, since it can
// cost log volume but never a denied write.
func ParseMode(raw string) Mode {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case ModeOff:
		return ModeOff
	case ModeEnforce:
		return ModeEnforce
	default:
		return DefaultMode
	}
}

// Current reads the process environment. It is a function rather than a
// variable so tests and operators can both change it without touching package
// state.
func Current() Mode {
	return ParseMode(os.Getenv(ModeEnvVar))
}

// Observes reports whether the guard should evaluate a transition at all.
func (m Mode) Observes() bool {
	return m != ModeOff
}

// Rejects reports whether this mode is allowed to deny a write. Only enforce
// may; shadow and off must leave every transition untouched.
func (m Mode) Rejects() bool {
	return m == ModeEnforce
}
