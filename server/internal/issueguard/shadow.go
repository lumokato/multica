package issueguard

// ShadowEvent is one observed status transition, shaped for structured
// logging. It exists so the handler can emit a stable field set without
// importing policy details, and so tests can assert on the record instead of
// on log text.
type ShadowEvent struct {
	Mode        Mode
	IssueID     string
	IssueNumber int32
	From        string
	To          string
	Violations  []Violation
	WouldBlock  bool
}

// NewShadowEvent builds the record for one transition.
func NewShadowEvent(mode Mode, issueID string, issueNumber int32, from, to string, violations []Violation) ShadowEvent {
	return ShadowEvent{
		Mode:        mode,
		IssueID:     issueID,
		IssueNumber: issueNumber,
		From:        from,
		To:          to,
		Violations:  violations,
		WouldBlock:  Blocks(violations),
	}
}

// Codes lists the violation codes in the order they were found, including
// warnings: shadow mode exists to collect both, and a code that never appears
// is itself information.
func (e ShadowEvent) Codes() []string {
	return Codes(e.Violations)
}

// Fields renders the record as slog key/value pairs.
func (e ShadowEvent) Fields() []any {
	return []any{
		"mode", string(e.Mode),
		"issue_id", e.IssueID,
		"issue_number", e.IssueNumber,
		"from", e.From,
		"to", e.To,
		"codes", e.Codes(),
		"would_block", e.WouldBlock,
	}
}

// Codes extracts violation codes, preserving order and duplicates removed.
func Codes(vs []Violation) []string {
	if len(vs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(vs))
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if v.Code == "" {
			continue
		}
		if _, dup := seen[v.Code]; dup {
			continue
		}
		seen[v.Code] = struct{}{}
		out = append(out, v.Code)
	}
	return out
}
