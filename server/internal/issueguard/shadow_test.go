package issueguard

import (
	"reflect"
	"testing"
)

func TestNewShadowEventRecordsWouldBlock(t *testing.T) {
	violations := CheckStatusChange(StatusChange{From: "in_progress", To: "done"})
	event := NewShadowEvent(ModeShadow, "issue-1", 999, "in_progress", "done", violations)

	if !event.WouldBlock {
		t.Fatalf("a done transition with no brief must be recorded as would_block")
	}
	if event.Mode != ModeShadow {
		t.Fatalf("mode = %q", event.Mode)
	}
	if event.From != "in_progress" || event.To != "done" {
		t.Fatalf("transition lost: %q -> %q", event.From, event.To)
	}
	if got := event.Codes(); len(got) == 0 || got[0] != "brief_missing" {
		t.Fatalf("codes = %v, want brief_missing first", got)
	}
}

func TestShadowEventFieldsAreStable(t *testing.T) {
	event := NewShadowEvent(ModeShadow, "issue-1", 999, "todo", "in_progress", nil)
	fields := event.Fields()

	wantKeys := []any{"mode", "issue_id", "issue_number", "from", "to", "codes", "would_block"}
	if len(fields) != 2*len(wantKeys) {
		t.Fatalf("field count = %d, want %d (%v)", len(fields), 2*len(wantKeys), fields)
	}
	for i, want := range wantKeys {
		if fields[i*2] != want {
			t.Fatalf("field %d key = %v, want %v", i, fields[i*2], want)
		}
	}
	if event.WouldBlock {
		t.Fatalf("a start transition only warns, so would_block must be false")
	}
}

func TestCodesDeduplicatesAndKeepsOrder(t *testing.T) {
	got := Codes([]Violation{
		blocking("b", "x"),
		warning("a", "y"),
		blocking("b", "z"),
		nil2(),
	})
	want := []string{"b", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codes() = %v, want %v", got, want)
	}
}

func TestCodesEmpty(t *testing.T) {
	if got := Codes(nil); got != nil {
		t.Fatalf("Codes(nil) = %v, want nil so the log field stays absent", got)
	}
}

// nil2 keeps the dedupe case readable next to a real zero value.
func nil2() Violation { return Violation{} }
