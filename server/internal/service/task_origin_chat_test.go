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
		Processing: "已交主智能体处理",
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
		"⟦任务结果⟧ 📋 子任务结果 · PER-303 · in_review（待负责人审核） · 由「Agent \\| One」产出（不是本会话回复）",
		"处理状态：已交主智能体处理",
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
		Processing: "仅展示",
	}
	got := formatOriginChatResultWithPrefix("PER", p)
	for _, want := range []string{"处理状态：仅展示", "| run | failed · attempt 2", "| failure_reason | agent_error.provider |", "| error | provider said [链接已省略] |", "queued — · 未启动", "⟦任务结果结束⟧"} {
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

// LS-206: a result that needs action is handed to the origin chat agent on the
// sub-issue. The card stays display-only, and a redelivery of the same run must
// not wake the agent twice.
func TestDeliverIssueResultToOriginChatWakesOriginAgentOnce(t *testing.T) {
	ctx := context.Background()
	env := newOriginChatTestEnv(t)
	fx := env.fx
	issueID := fx.Issue(t, "origin chat handoff", testutil.Cols{
		"status":        "in_review",
		"origin_type":   "agent_create",
		"origin_id":     env.originTaskID,
		"creator_type":  "agent",
		"creator_id":    env.workerAgent,
		"assignee_type": "agent",
		"assignee_id":   env.workerAgent,
	})
	runID := fx.Task(t, env.workerAgent, testutil.Cols{
		"runtime_id": env.runtimeID,
		"issue_id":   issueID,
		"status":     "completed",
		"result":     []byte(`{"output":"finished the work"}`),
	})

	svc := NewTaskService(db.New(fx.Pool), fx.Pool, nil, events.New())
	run, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	svc.DeliverIssueResultToOriginChat(ctx, run)
	svc.DeliverIssueResultToOriginChat(ctx, run)

	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, env.mainAgent); got != 1 {
		t.Fatalf("origin agent has %d runs on the sub-issue, want exactly 1", got)
	}
	if got := fx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1 AND message_kind='task_result'`, env.sessionID); got != 1 {
		t.Fatalf("session has %d result cards, want 1", got)
	}
	if got := fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND source_task_id=$2 AND author_type='system' AND content LIKE '⟦任务结果交接⟧%'`, issueID, runID); got != 1 {
		t.Fatalf("issue has %d handoff records, want 1", got)
	}
	// The origin turn is the only chat task: handing a result back must never
	// open a chat turn.
	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1`, env.sessionID); got != 1 {
		t.Fatalf("session has %d tasks, want only the origin turn", got)
	}
	var content string
	fx.QueryRow(t, `SELECT content FROM chat_message WHERE chat_session_id=$1 AND message_kind='task_result'`, env.sessionID).Scan(&content)
	if !strings.Contains(content, "处理状态：已交主智能体处理") {
		t.Fatalf("card does not report the handoff:\n%s", content)
	}
	// The woken run inherits the human who started the chat (through the
	// issue's agent_create origin), so the handoff does not invent authority.
	var originator string
	fx.QueryRow(t, `SELECT originator_user_id::text FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, env.mainAgent).Scan(&originator)
	if originator != fx.UserID {
		t.Fatalf("woken run originator = %q, want %q", originator, fx.UserID)
	}
}

// LS-206: ai/done needs no follow-up, so the card is display-only and no run is
// queued for the origin agent.
func TestDeliverIssueResultToOriginChatDoesNotWakeCompletedDone(t *testing.T) {
	ctx := context.Background()
	env := newOriginChatTestEnv(t)
	fx := env.fx
	issueID := fx.Issue(t, "origin chat done", testutil.Cols{
		"status":        "ai",
		"origin_type":   "agent_create",
		"origin_id":     env.originTaskID,
		"creator_type":  "agent",
		"creator_id":    env.workerAgent,
		"assignee_type": "agent",
		"assignee_id":   env.workerAgent,
	})
	runID := fx.Task(t, env.workerAgent, testutil.Cols{
		"runtime_id": env.runtimeID,
		"issue_id":   issueID,
		"status":     "completed",
		"result":     []byte(`{"output":"finished the work"}`),
	})

	svc := NewTaskService(db.New(fx.Pool), fx.Pool, nil, events.New())
	run, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	svc.DeliverIssueResultToOriginChat(ctx, run)

	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, env.mainAgent); got != 0 {
		t.Fatalf("origin agent has %d runs for an ai issue, want 0", got)
	}
	if got := fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND source_task_id=$2`, issueID, runID); got != 0 {
		t.Fatalf("issue has %d handoff records for an ai issue, want 0", got)
	}
	var content string
	fx.QueryRow(t, `SELECT content FROM chat_message WHERE chat_session_id=$1 AND message_kind='task_result'`, env.sessionID).Scan(&content)
	if !strings.Contains(content, "处理状态：仅展示") {
		t.Fatalf("card does not report display-only:\n%s", content)
	}
}

// LS-206: a failed run on a blocked issue wakes the origin agent.
func TestDeliverIssueResultToOriginChatWakesBlockedFailure(t *testing.T) {
	ctx := context.Background()
	env := newOriginChatTestEnv(t)
	fx := env.fx
	issueID := fx.Issue(t, "origin chat blocked", testutil.Cols{
		"status":        "blocked",
		"origin_type":   "agent_create",
		"origin_id":     env.originTaskID,
		"creator_type":  "agent",
		"creator_id":    env.workerAgent,
		"assignee_type": "agent",
		"assignee_id":   env.workerAgent,
	})
	runID := fx.Task(t, env.workerAgent, testutil.Cols{
		"runtime_id":     env.runtimeID,
		"issue_id":       issueID,
		"status":         "failed",
		"failure_reason": "agent_error.provider",
		"error":          "provider refused",
	})

	svc := NewTaskService(db.New(fx.Pool), fx.Pool, nil, events.New())
	run, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	svc.DeliverIssueResultToOriginChat(ctx, run)

	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, env.mainAgent); got != 1 {
		t.Fatalf("origin agent has %d runs for a blocked failure, want 1", got)
	}
	var content string
	fx.QueryRow(t, `SELECT content FROM comment WHERE issue_id=$1 AND source_task_id=$2 AND author_type='system' AND content LIKE '⟦任务结果交接⟧%'`, issueID, runID).Scan(&content)
	if !strings.Contains(content, "任务失败") {
		t.Fatalf("handoff record does not state the failure reason:\n%s", content)
	}
}

// LS-206: the claim recap only includes cards that arrived after the previous
// chat turn, newest first at the query level.
func TestListTaskResultMessagesAfterPreviousChatTask(t *testing.T) {
	ctx := context.Background()
	env := newOriginChatTestEnv(t)
	fx := env.fx
	// A dedicated session keeps this test independent of the fixture's origin
	// turn, which would otherwise be the newest "other" task.
	sessionID := fx.ChatSession(t, env.mainAgent)
	previousTask := fx.Task(t, env.mainAgent, testutil.Cols{
		"runtime_id":      env.runtimeID,
		"chat_session_id": sessionID,
		"status":          "completed",
		"created_at":      testutil.Raw("now() - interval '10 minutes'"),
	})
	currentTask := fx.Task(t, env.mainAgent, testutil.Cols{
		"runtime_id":      env.runtimeID,
		"chat_session_id": sessionID,
		"status":          "queued",
		"created_at":      testutil.Raw("now()"),
	})
	insertCard := func(content string, age string) string {
		return fx.Insert(t, "chat_message", testutil.Cols{
			"chat_session_id": sessionID,
			"role":            "assistant",
			"content":         content,
			"message_kind":    "task_result",
			"created_at":      testutil.Raw(age),
		})
	}
	oldCard := insertCard("⟦任务结果⟧ 📋 子任务结果 · PER-1 · ai · 由「worker」产出（不是本会话回复）\n处理状态：仅展示", "now() - interval '20 minutes'")
	newCard := insertCard("⟦任务结果⟧ 📋 子任务结果 · PER-2 · in_review · 由「worker」产出（不是本会话回复）\n处理状态：已交主智能体处理", "now() - interval '5 minutes'")

	svc := NewTaskService(db.New(fx.Pool), fx.Pool, nil, events.New())
	cards, err := svc.Queries.ListTaskResultMessagesAfterPreviousChatTask(ctx, db.ListTaskResultMessagesAfterPreviousChatTaskParams{
		ChatSessionID: util.MustParseUUID(sessionID),
		CurrentTaskID: util.MustParseUUID(currentTask),
		MaxRows:       5,
	})
	if err != nil {
		t.Fatalf("load recap cards: %v", err)
	}
	if len(cards) != 1 {
		t.Fatalf("recap returned %d cards, want 1", len(cards))
	}
	if got := util.UUIDToString(cards[0].ID); got != newCard {
		t.Fatalf("recap returned card %s, want the one after the previous turn (%s; old card %s; previous task %s)", got, newCard, oldCard, previousTask)
	}
}

type originChatTestEnv struct {
	fx           *testutil.Fixture
	runtimeID    string
	mainAgent    string
	workerAgent  string
	sessionID    string
	originTaskID string
}

func newOriginChatTestEnv(t *testing.T) originChatTestEnv {
	t.Helper()
	ctx := context.Background()
	pool := sharedTestPool(t)
	suffix := time.Now().UnixNano()
	fx := testutil.New(pool, "", "")
	fx.UserID = fx.User(t, "origin chat handoff", fmt.Sprintf("origin-chat-handoff-%d@example.test", suffix))
	fx.WorkspaceID = fx.Workspace(t, "origin chat handoff", fmt.Sprintf("origin-chat-handoff-%d", suffix))
	fx.Member(t, fx.WorkspaceID, fx.UserID, "owner")
	runtimeID := fx.Runtime(t, "origin chat handoff")
	mainAgent := fx.Agent(t, "origin chat main", runtimeID)
	workerAgent := fx.Agent(t, "origin chat worker", runtimeID)
	sessionID := fx.ChatSession(t, mainAgent)
	fx.Cleanup(t, "DELETE FROM chat_message WHERE chat_session_id=$1", sessionID)
	// The origin turn is the chat that created the sub-issue. Its originator is
	// the human the whole chain acts for.
	originTaskID := fx.Task(t, mainAgent, testutil.Cols{
		"runtime_id":          runtimeID,
		"chat_session_id":     sessionID,
		"status":              "completed",
		"originator_user_id":  fx.UserID,
		"accountable_user_id": fx.UserID,
	})
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return originChatTestEnv{
		fx:           fx,
		runtimeID:    runtimeID,
		mainAgent:    mainAgent,
		workerAgent:  workerAgent,
		sessionID:    sessionID,
		originTaskID: originTaskID,
	}
}

// LS-206: the handoff gate is exactly the member's set: non-success, blocked,
// and completed-awaiting-review. Completed ai/done and in-flight runs stay
// display-only.
func TestOriginChatResultNeedsHandoff(t *testing.T) {
	cases := []struct {
		name   string
		issue  db.Issue
		task   db.AgentTaskQueue
		expect bool
	}{
		{"failed", db.Issue{Status: "in_progress"}, db.AgentTaskQueue{Status: "failed"}, true},
		{"cancelled", db.Issue{Status: "in_progress"}, db.AgentTaskQueue{Status: "cancelled"}, true},
		{"failure reason", db.Issue{Status: "in_progress"}, db.AgentTaskQueue{Status: "completed", FailureReason: pgtype.Text{String: "agent_error", Valid: true}}, true},
		{"error text", db.Issue{Status: "in_progress"}, db.AgentTaskQueue{Status: "completed", Error: pgtype.Text{String: "boom", Valid: true}}, true},
		{"blocked", db.Issue{Status: "blocked"}, db.AgentTaskQueue{Status: "completed"}, true},
		{"completed in_review", db.Issue{Status: "in_review"}, db.AgentTaskQueue{Status: "completed"}, true},
		{"completed ai", db.Issue{Status: "ai"}, db.AgentTaskQueue{Status: "completed"}, false},
		{"completed done", db.Issue{Status: "done"}, db.AgentTaskQueue{Status: "completed"}, false},
		{"completed todo", db.Issue{Status: "todo"}, db.AgentTaskQueue{Status: "completed"}, false},
		{"running", db.Issue{Status: "in_review"}, db.AgentTaskQueue{Status: "running"}, false},
	}
	for _, tc := range cases {
		if got := originChatResultNeedsHandoff(tc.issue, tc.task); got != tc.expect {
			t.Errorf("%s: needs handoff = %v, want %v", tc.name, got, tc.expect)
		}
	}
}

func TestTaskResultCardSummaryKeepsHeaderAndTruncatesOutput(t *testing.T) {
	content := "⟦任务结果⟧ 📋 子任务结果 · PER-303 · in_review · 由「worker」产出（不是本会话回复）\n处理状态：已交主智能体处理\n\n| 项 | 值 |\n|---|---|\n| issue | PER-303 |\n\n**执行体输出**\n\n" + strings.Repeat("x", 2000) + "\n⟦任务结果结束⟧"
	got := TaskResultCardSummary(content, 100)
	if !strings.HasPrefix(got, "子任务结果 · PER-303 · in_review · 由「worker」产出（不是本会话回复）\n") {
		t.Fatalf("summary header = %q", got)
	}
	if strings.Contains(got, "| 项 | 值 |") {
		t.Fatalf("summary kept the table:\n%s", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("summary did not truncate output:\n%s", got)
	}
	if runes := []rune(got); len(runes) > 100+len([]rune("子任务结果 · PER-303 · in_review · 由「worker」产出（不是本会话回复）\n")) {
		t.Fatalf("summary exceeds its output budget: %d runes", len(runes))
	}
}

// LS-206: a sub-issue run performed by the origin chat agent itself is not a
// handoff. Waking it would make every in_review completion start another run on
// the same issue, so the card stays display-only.
func TestDeliverIssueResultToOriginChatDoesNotWakeForOwnRun(t *testing.T) {
	ctx := context.Background()
	env := newOriginChatTestEnv(t)
	fx := env.fx
	issueID := fx.Issue(t, "origin chat own run", testutil.Cols{
		"status":        "in_review",
		"origin_type":   "agent_create",
		"origin_id":     env.originTaskID,
		"creator_type":  "agent",
		"creator_id":    env.mainAgent,
		"assignee_type": "agent",
		"assignee_id":   env.mainAgent,
	})
	runID := fx.Task(t, env.mainAgent, testutil.Cols{
		"runtime_id": env.runtimeID,
		"issue_id":   issueID,
		"status":     "completed",
		"result":     []byte(`{"output":"finished the work"}`),
	})

	svc := NewTaskService(db.New(fx.Pool), fx.Pool, nil, events.New())
	run, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(runID))
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	svc.DeliverIssueResultToOriginChat(ctx, run)

	if got := fx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, env.mainAgent); got != 1 {
		t.Fatalf("origin agent has %d runs on the issue, want only the run that produced the result", got)
	}
	if got := fx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND source_task_id=$2`, issueID, runID); got != 0 {
		t.Fatalf("own run produced %d handoff records, want 0", got)
	}
}
