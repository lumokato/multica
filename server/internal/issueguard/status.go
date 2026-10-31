package issueguard

// Package-level status guard (architecture plan v6, backend change #1).
//
// The guard judges one attempted issue status transition and returns the
// violations it found. It is pure: no database, no clock, no I/O, so the same
// table can run in shadow mode (log only), in tests, and later behind the
// enforcement switch without behaviour drifting between them.
//
// Two severities exist on purpose. "blocking" is what enforcement rejects;
// "warning" is what shadow mode collects so we can tune the thresholds before
// anything is denied. A violation never changes the transition by itself — the
// caller decides, which is what keeps the rollback switch honest.

import (
	"fmt"
	"strings"
	"time"
)

// RiskLevel classifies the blast radius of a task, which decides who may
// approve its completion. See architecture plan v6 §8 (D8).
type RiskLevel string

const (
	// RiskLow covers documentation, indexes, archiving and read-only analysis.
	RiskLow RiskLevel = "low"
	// RiskMedium covers merges, restarts, migrations and batch writes.
	RiskMedium RiskLevel = "medium"
	// RiskHigh covers deploys, deletions, outbound messages, production
	// configuration and anything that spends money.
	RiskHigh RiskLevel = "high"
)

// TaskBrief is the four-field task statement (architecture plan v6, D7). The
// "known pitfalls" field was deliberately dropped: it duplicated the rule
// injection layer and drifted out of date.
type TaskBrief struct {
	// Goal is one sentence that can be judged true or false.
	Goal string
	// Assertion is the executable check: command plus expected result.
	Assertion string
	// Artifacts records where the output goes: repository, branch, and what
	// temporary state must be cleaned up.
	Artifacts string
	// Constraints records what may not be changed and who set the priority.
	Constraints string
}

// Evidence is one recorded assertion run attached to the issue.
type Evidence struct {
	// Command is the command that ran. Empty evidence cannot be trusted.
	Command string
	// ExitCode is the process exit status of that command.
	ExitCode int
	// DisplayChecked marks evidence that sampled the user-facing surface. It
	// is what separates "the job exited 0" from "the change is visible".
	DisplayChecked bool
	// At is when the run happened; zero means unknown.
	At time.Time
}

// BlockedReason is the three-part explanation a blocked transition requires.
type BlockedReason struct {
	// Reason is what is stuck.
	Reason string
	// DependsOn names the concrete external dependency.
	DependsOn string
	// WakeupCondition is the fact that, once true, resumes the work.
	WakeupCondition string
}

// StatusChange is one attempted transition, as the caller observed it.
type StatusChange struct {
	From string
	To   string
	// Brief is nil when the issue carries no task brief at all.
	Brief *TaskBrief
	// Evidence lists assertion runs recorded on the issue, oldest first.
	Evidence []Evidence
	// ReviewerApproved is true when an independent reviewer already approved
	// this specific transition.
	ReviewerApproved bool
	// Risk defaults to RiskLow when empty, so a missing classification cannot
	// silently bypass the high-risk requirements.
	Risk RiskLevel
	// Blocked is required only when To is "blocked".
	Blocked *BlockedReason
}

// Severity says what the caller should do with a violation.
type Severity string

const (
	// SeverityBlocking is rejected once enforcement is on.
	SeverityBlocking Severity = "blocking"
	// SeverityWarning is recorded only; it exists to tune thresholds.
	SeverityWarning Severity = "warning"
)

// Violation is one reason a transition does not meet the completion contract.
type Violation struct {
	Code     string
	Message  string
	Severity Severity
}

// Canonical status keys, mirrored from internal/issuestatus so the guard can
// be tested without importing the catalog (which needs a database).
const (
	statusInProgress = "in_progress"
	statusInReview   = "in_review"
	statusDone       = "done"
	statusAI         = "ai"
	statusBlocked    = "blocked"
	statusCancelled  = "cancelled"
)

// CheckStatusChange judges one transition. An empty result means the
// transition satisfies the contract.
func CheckStatusChange(c StatusChange) []Violation {
	risk := c.Risk
	if risk == "" {
		risk = RiskLow
	}

	switch c.To {
	case statusDone, statusAI:
		return checkCompletion(c, risk)
	case statusBlocked:
		return checkBlocked(c)
	case statusInReview:
		return checkReview(c)
	case statusInProgress:
		return checkStart(c)
	case statusCancelled:
		// Cancelling is always allowed: refusing it would trap work that must
		// stop, including work the guard itself is blocking.
		return nil
	default:
		return []Violation{{
			Code:     "unknown_status",
			Message:  fmt.Sprintf("status %q is not part of the completion contract; no checks applied", c.To),
			Severity: SeverityWarning,
		}}
	}
}

func checkCompletion(c StatusChange, risk RiskLevel) []Violation {
	var out []Violation

	if c.Brief == nil {
		out = append(out, blocking("brief_missing", "issue has no task brief; a completion needs goal and assertion"))
		return out
	}

	if strings.TrimSpace(c.Brief.Goal) == "" {
		out = append(out, blocking("goal_missing", "task brief has no goal sentence"))
	}
	if strings.TrimSpace(c.Brief.Assertion) == "" {
		out = append(out, blocking("assertion_missing", "task brief has no executable assertion, so completion cannot be judged"))
	}
	if strings.TrimSpace(c.Brief.Artifacts) == "" {
		out = append(out, warning("artifacts_missing", "task brief does not say where the output went or what was cleaned up"))
	}
	if strings.TrimSpace(c.Brief.Constraints) == "" {
		out = append(out, warning("constraints_missing", "task brief does not record what may not be changed"))
	}

	passed := 0
	withDisplay := 0
	for _, e := range c.Evidence {
		if strings.TrimSpace(e.Command) == "" {
			out = append(out, warning("evidence_without_command", "evidence entry has no command, so it cannot be replayed"))
			continue
		}
		if e.ExitCode == 0 {
			passed++
			if e.DisplayChecked {
				withDisplay++
			}
		}
	}

	switch {
	case len(c.Evidence) == 0:
		out = append(out, blocking("evidence_missing", "no assertion evidence recorded; exit status alone is not evidence"))
	case passed == 0:
		out = append(out, blocking("evidence_failed", "every recorded assertion exited non-zero"))
	}

	if risk == RiskHigh {
		if passed > 0 && withDisplay == 0 {
			out = append(out, blocking("display_not_checked", "high-risk task passed but no evidence sampled the user-facing surface"))
		}
		if !c.ReviewerApproved {
			out = append(out, blocking("reviewer_missing", "high-risk completion needs an independent reviewer approval"))
		}
	}

	return out
}

func checkReview(c StatusChange) []Violation {
	if c.Brief == nil {
		return []Violation{warning("brief_missing", "moving to review without a task brief leaves the reviewer nothing to check against")}
	}
	if strings.TrimSpace(c.Brief.Assertion) == "" {
		return []Violation{warning("assertion_missing", "review requested without an executable assertion")}
	}
	return nil
}

func checkStart(c StatusChange) []Violation {
	if c.Brief == nil {
		return []Violation{warning("brief_missing", "starting work without a task brief")}
	}
	if strings.TrimSpace(c.Brief.Assertion) == "" {
		return []Violation{warning("assertion_missing", "starting work without an executable assertion; completion will be unjudgeable")}
	}
	return nil
}

func checkBlocked(c StatusChange) []Violation {
	if c.Blocked == nil {
		return []Violation{blocking("blocked_reason_missing", "blocked needs reason, dependency and wakeup condition")}
	}
	var out []Violation
	if strings.TrimSpace(c.Blocked.Reason) == "" {
		out = append(out, blocking("blocked_reason_missing", "blocked has no stated reason"))
	}
	if strings.TrimSpace(c.Blocked.DependsOn) == "" {
		out = append(out, blocking("blocked_dependency_missing", "blocked does not name the external dependency"))
	}
	if strings.TrimSpace(c.Blocked.WakeupCondition) == "" {
		out = append(out, blocking("blocked_wakeup_missing", "blocked does not say which fact resumes the work"))
	}
	return out
}

// Blocks reports whether any violation must be rejected once enforcement is
// on. Shadow mode ignores this and logs everything.
func Blocks(vs []Violation) bool {
	for _, v := range vs {
		if v.Severity == SeverityBlocking {
			return true
		}
	}
	return false
}

func blocking(code, msg string) Violation {
	return Violation{Code: code, Message: msg, Severity: SeverityBlocking}
}

func warning(code, msg string) Violation {
	return Violation{Code: code, Message: msg, Severity: SeverityWarning}
}
