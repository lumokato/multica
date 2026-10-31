package issueguard

import "testing"

func TestParseMode(t *testing.T) {
	tests := []struct {
		raw  string
		want Mode
	}{
		{"", ModeShadow},
		{"off", ModeOff},
		{"OFF", ModeOff},
		{" off ", ModeOff},
		{"shadow", ModeShadow},
		{"enforce", ModeEnforce},
		{"Enforce", ModeEnforce},
		{"bogus", ModeShadow},
	}
	for _, tc := range tests {
		if got := ParseMode(tc.raw); got != tc.want {
			t.Fatalf("ParseMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestModeObservesAndRejects(t *testing.T) {
	tests := []struct {
		mode        Mode
		wantObserve bool
		wantReject  bool
	}{
		{ModeOff, false, false},
		{ModeShadow, true, false},
		{ModeEnforce, true, true},
	}
	for _, tc := range tests {
		if got := tc.mode.Observes(); got != tc.wantObserve {
			t.Fatalf("%q.Observes() = %v, want %v", tc.mode, got, tc.wantObserve)
		}
		if got := tc.mode.Rejects(); got != tc.wantReject {
			t.Fatalf("%q.Rejects() = %v, want %v", tc.mode, got, tc.wantReject)
		}
	}
}

func TestCurrentReadsEnv(t *testing.T) {
	t.Setenv(ModeEnvVar, "off")
	if got := Current(); got != ModeOff {
		t.Fatalf("Current() with %s=off = %q", ModeEnvVar, got)
	}
	t.Setenv(ModeEnvVar, "")
	if got := Current(); got != ModeShadow {
		t.Fatalf("Current() with unset = %q, want shadow", got)
	}
}
