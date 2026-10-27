package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
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
	for _, want := range []string{"| run | failed · attempt 2", "| failure_reason | agent_error.provider |", "| error | provider said [链接已省略] |", "queued — · 未启动", "⟦任务结果结束⟧"} {
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
	if got != "queued —" {
		t.Fatalf("time = %q", got)
	}
}

func TestOriginMessageTimingCancelledBeforeStartOmitsExecution(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	got := formatOriginTime(db.AgentTaskQueue{
		Status:      "cancelled",
		CreatedAt:   pgtype.Timestamptz{Time: base, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: base.Add(3 * time.Second), Valid: true},
	}, nil)
	if got != "queued 3s · 未启动" {
		t.Fatalf("cancelled-before-start time = %q", got)
	}
	if strings.Contains(got, "?") || strings.Contains(got, "exec") {
		t.Fatalf("cancelled-before-start time must not invent an execution segment: %q", got)
	}
}

func TestOriginMessageTimingCompletedWithoutStartTimeOmitsExecution(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	got := formatOriginTime(db.AgentTaskQueue{
		Status:      "completed",
		CreatedAt:   pgtype.Timestamptz{Time: base, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: base.Add(45 * time.Second), Valid: true},
	}, nil)
	if got != "queued 45s · 未启动" {
		t.Fatalf("completed-without-start time = %q", got)
	}
}

func TestOriginMessageTimingNeverEmitsQuestionMark(t *testing.T) {
	base := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	cases := []db.AgentTaskQueue{
		{Status: "queued", CreatedAt: pgtype.Timestamptz{Time: base, Valid: true}},
		{Status: "running", CreatedAt: pgtype.Timestamptz{Time: base, Valid: true}, StartedAt: pgtype.Timestamptz{Time: base.Add(time.Second), Valid: true}},
		{Status: "completed", CreatedAt: pgtype.Timestamptz{Time: base, Valid: true}, StartedAt: pgtype.Timestamptz{Time: base.Add(time.Second), Valid: true}, CompletedAt: pgtype.Timestamptz{Time: base.Add(4 * time.Second), Valid: true}},
		{Status: "failed", StartedAt: pgtype.Timestamptz{Time: base, Valid: true}},
		{Status: "cancelled", CreatedAt: pgtype.Timestamptz{Time: base, Valid: true}},
	}
	for _, task := range cases {
		if got := formatOriginTime(task, nil); strings.Contains(got, "?") {
			t.Fatalf("time for %s contains a placeholder: %q", task.Status, got)
		}
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
	got := formatOriginTime(db.AgentTaskQueue{
		Status:      "completed",
		StartedAt:   pgtype.Timestamptz{Time: base, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: base.Add(10 * time.Second), Valid: true},
	}, []db.TaskMessage{
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

// The card's identity is the idempotency key: the same run delivering to the
// same session must always derive the same primary key, and a different run
// (or session) must not.
func TestOriginChatResultMessageIDIsStablePerDelivery(t *testing.T) {
	// originUUID is constant by design (it only fills in "any id"), so distinct
	// keys here are built from uuid.NewSHA1 instead.
	key := func(name string) pgtype.UUID {
		return pgtype.UUID{Bytes: [16]byte(uuid.NewSHA1(uuid.NameSpaceOID, []byte(name))), Valid: true}
	}
	session, task := key("session"), key("task")
	first := originChatResultMessageID(session, task)
	if second := originChatResultMessageID(session, task); first != second {
		t.Fatalf("same delivery derived two ids: %v vs %v", first, second)
	}
	if !first.Valid {
		t.Fatal("derived id is not valid")
	}
	if other := originChatResultMessageID(session, key("other task")); other == first {
		t.Fatalf("two runs share a delivery key: %v", first)
	}
	if other := originChatResultMessageID(key("other session"), task); other == first {
		t.Fatalf("two sessions share a delivery key: %v", first)
	}
}

// LS-173: a delivered result must be one task-less assistant card per run, and
// redelivering the same run must not append a second card or enqueue a chat
// turn. The pre-fix path went through SendDirectChatMessage, which opened a
// task per card and therefore re-ran the agent.
func TestDeliverIssueResultToOriginChatWritesOneTasklessCard(t *testing.T) {
	ctx := context.Background()
	pool := sharedTestPool(t)
	suffix := time.Now().UnixNano()
	fx := testutil.New(pool, "", "")
	fx.UserID = fx.User(t, "origin chat result", fmt.Sprintf("origin-chat-result-%d@example.test", suffix))
	fx.WorkspaceID = fx.Workspace(t, "origin chat result", fmt.Sprintf("origin-chat-result-%d", suffix))
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "origin chat result")
	agentID := fx.Agent(t, "origin chat result", runtimeID)
	sessionID := fx.ChatSession(t, agentID)
	fx.Cleanup(t, "DELETE FROM chat_message WHERE chat_session_id=$1", sessionID)

	// The chat turn that created the issue, so the result has somewhere to go.
	originTaskID := fx.Task(t, agentID, testutil.Cols{
		"runtime_id":      runtimeID,
		"chat_session_id": sessionID,
		"status":          "completed",
	})
	issueID := fx.Issue(t, "origin chat result", testutil.Cols{
		"status":        "in_review",
		"origin_type":   "agent_create",
		"origin_id":     originTaskID,
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	runID := fx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
		"status":     "completed",
		"result":     []byte(`{"output":"finished the work"}`),
	})

	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	run, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}

	svc.DeliverIssueResultToOriginChat(ctx, run)
	svc.DeliverIssueResultToOriginChat(ctx, run)

	rows, err := pool.Query(ctx, `SELECT role, task_id IS NULL, content FROM chat_message WHERE chat_session_id=$1`, sessionID)
	if err != nil {
		t.Fatalf("read cards: %v", err)
	}
	defer rows.Close()
	var cards int
	for rows.Next() {
		var role, content string
		var taskless bool
		if err := rows.Scan(&role, &taskless, &content); err != nil {
			t.Fatalf("scan card: %v", err)
		}
		cards++
		if role != "assistant" {
			t.Fatalf("card role = %q, want assistant (a task-less user row would be adopted by the next input batch)", role)
		}
		if !taskless {
			t.Fatalf("card owns a task, so it opened a chat turn")
		}
		if !strings.Contains(content, "⟦任务结果⟧") {
			t.Fatalf("card content = %q", content)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cards: %v", err)
	}
	if cards != 1 {
		t.Fatalf("delivered %d cards for one run, want 1", cards)
	}

	var chatTasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1`, sessionID).Scan(&chatTasks); err != nil {
		t.Fatalf("count chat tasks: %v", err)
	}
	if chatTasks != 1 {
		t.Fatalf("session has %d tasks after delivery, want only the origin turn (1)", chatTasks)
	}
}
