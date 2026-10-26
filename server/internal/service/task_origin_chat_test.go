package service

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func originUUID(s string) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}
}

func TestFormatOriginChatResultComplete(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	p := originChatPayload{
		Task: db.AgentTaskQueue{
			ID: originUUID("task"), Status: "completed", Attempt: 1,
			CreatedAt:            pgtype.Timestamptz{Time: base, Valid: true},
			StartedAt:            pgtype.Timestamptz{Time: base.Add(2 * time.Second), Valid: true},
			CompletedAt:          pgtype.Timestamptz{Time: base.Add(12 * time.Second), Valid: true},
			TriggerEvidenceKind:  pgtype.Text{String: "delegation", Valid: true},
			TriggerEvidenceRefID: originUUID("trigger"),
		},
		Issue:      db.Issue{ID: originUUID("issue"), Number: 303, Status: "in_review"},
		OriginTask: db.AgentTaskQueue{ChatSessionID: originUUID("chat")},
		Agent:      db.Agent{ID: originUUID("agent"), Name: "Agent | One"},
		Usage:      []db.TaskUsage{{Provider: "pi", Model: "gpt-6.1-sol", InputTokens: 92, OutputTokens: 19, CacheReadTokens: 26, CacheWriteTokens: 4}},
		Messages: []db.TaskMessage{
			{Type: "text", CreatedAt: pgtype.Timestamptz{Time: base.Add(3 * time.Second), Valid: true}},
			{Type: "tool_use", CallID: pgtype.Text{String: "c1", Valid: true}, CreatedAt: pgtype.Timestamptz{Time: base.Add(4 * time.Second), Valid: true}},
			{Type: "tool_result", CallID: pgtype.Text{String: "c1", Valid: true}, CreatedAt: pgtype.Timestamptz{Time: base.Add(7 * time.Second), Valid: true}},
			{Type: "text", CreatedAt: pgtype.Timestamptz{Time: base.Add(9 * time.Second), Valid: true}},
		},
		Output: "done",
		PRURL:  "https://github.com/lumokato/multica/pull/9",
	}
	got := formatOriginChatResultWithPrefix("PER", p)
	for _, want := range []string{
		"⟦任务结果⟧ 📋 PER-303",
		"| issue | PER-303 · in_review（待负责人审核） · ",
		"| model | gpt-6.1-sol · pi · in 92 / out 19 / cache_read 26 / cache_write 4 |",
		"queued 2s · exec 10s · turns 2 · model≈6s（中位 2s，派生） · tools≈3s（最长 3s，派生）",
		"| pr | multica #9 |",
		"**执行体输出**\n\ndone",
		"⟦任务结果结束⟧",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("result missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "http://") || strings.Contains(got, "https://") || !strings.Contains(got, "| agent | Agent \\| One · ") {
		t.Fatalf("result contains a URL or malformed table cell:\n%s", got)
	}
}

func TestFormatOriginChatResultFailureAndNoOptionalFields(t *testing.T) {
	p := originChatPayload{
		Task:       db.AgentTaskQueue{ID: originUUID("task"), Status: "failed", Attempt: 2},
		Issue:      db.Issue{ID: originUUID("issue"), Number: 304, Status: "cancelled"},
		OriginTask: db.AgentTaskQueue{ChatSessionID: originUUID("chat")},
		Agent:      db.Agent{ID: originUUID("agent"), Name: "Agent"},
		Failure:    "agent_error.provider",
		Error:      "provider said https://secret.invalid/token",
	}
	got := formatOriginChatResultWithPrefix("PER", p)
	for _, want := range []string{"| run | failed · attempt 2", "| failure_reason | agent_error.provider |", "| error | provider said [链接已省略] |", "queued ? · exec ?", "⟦任务结果结束⟧"} {
		if !strings.Contains(got, want) {
			t.Fatalf("result missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "| model |") || strings.Contains(got, "| pr |") || strings.Contains(got, "| from |") {
		t.Fatalf("optional rows unexpectedly present:\n%s", got)
	}
}

func TestOriginChatEmptyPayloadFallsBackToLastTextOrComment(t *testing.T) {
	payload := newOriginChatPayload(
		db.AgentTaskQueue{Status: "completed", Attempt: 1},
		db.Issue{Number: 306, Status: "done"},
		db.AgentTaskQueue{}, db.Agent{},
		[]byte(`{"pr_url":"","output":"","session_id":"session","work_dir":"/Users/tau/work"}`), "", "",
	)
	messages := []db.TaskMessage{{Seq: 1, Type: "text", Content: pgtype.Text{String: "last assistant text", Valid: true}}}
	if got := chooseOriginChatOutput(payload.Output, messages, "last agent comment", "PER-306"); got != "last assistant text" {
		t.Fatalf("text fallback = %q", got)
	}
	if got := chooseOriginChatOutput("", nil, "last agent comment", "PER-306"); got != "last agent comment" {
		t.Fatalf("comment fallback = %q", got)
	}
	if got := chooseOriginChatOutput("", nil, "", "PER-306"); got != "本次 run 无文本输出，见 issue PER-306 评论" {
		t.Fatalf("empty fallback = %q", got)
	}
	got := originChatResultBody([]byte("See https://github.com/example/pr/1 and /Users/tau/work"))
	if !strings.Contains(got, "https://github.com/example/pr/1") || strings.Contains(got, "/Users/") {
		t.Fatalf("body URL should remain while local path is removed: %q", got)
	}
}

func TestOriginMessageTimingWithoutMessages(t *testing.T) {
	got := formatOriginTime(db.AgentTaskQueue{}, nil)
	if got != "queued ?" {
		t.Fatalf("time = %q", got)
	}
}

func TestOriginMessageTimingRunningOmitsExecutionAndDerivedDurations(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	got := formatOriginTime(db.AgentTaskQueue{
		Status: "running", CreatedAt: pgtype.Timestamptz{Time: base, Valid: true}, StartedAt: pgtype.Timestamptz{Time: base.Add(time.Second), Valid: true},
	}, []db.TaskMessage{{Type: "text", CreatedAt: pgtype.Timestamptz{Time: base.Add(2 * time.Second), Valid: true}}, {Type: "text", CreatedAt: pgtype.Timestamptz{Time: base.Add(3 * time.Second), Valid: true}}})
	if got != "queued 1s" {
		t.Fatalf("running time = %q", got)
	}
}

func TestOriginMessageTimingFiltersSubsecondIntervals(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	got := formatOriginTime(db.AgentTaskQueue{Status: "completed"}, []db.TaskMessage{
		{Type: "text", CreatedAt: pgtype.Timestamptz{Time: base, Valid: true}},
		{Type: "text", CreatedAt: pgtype.Timestamptz{Time: base.Add(500 * time.Millisecond), Valid: true}},
		{Type: "text", CreatedAt: pgtype.Timestamptz{Time: base.Add(3 * time.Second), Valid: true}},
	})
	if !strings.Contains(got, "model≈2s（中位 2s，派生）") {
		t.Fatalf("filtered timing = %q", got)
	}
}

func TestOriginTriggerAssignmentDoesNotExposeIssueID(t *testing.T) {
	id := originUUID("issue")
	got := formatOriginTrigger(db.AgentTaskQueue{IssueID: id, TriggerEvidenceKind: pgtype.Text{String: "issue_assignment", Valid: true}, TriggerEvidenceRefID: id})
	if got != "assignment" {
		t.Fatalf("trigger = %q", got)
	}
}
