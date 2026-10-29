package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// taskResultHandoffCommentMarker prefixes the platform-authored handoff record
// left on a sub-issue. The comment type must stay one of the existing
// comment_type_check values (no migration for LS-206), so the marker — not a
// new type — is what separates this record from the delegated-failure signal,
// which also carries source_task_id.
const taskResultHandoffCommentMarker = "⟦任务结果交接⟧"

// originChatResultNeedsHandoff reports whether a delivered sub-issue result
// requires the origin chat agent to act on the issue (LS-206).
//
// Only the two outcomes the member explicitly asked for are handed back:
// non-success, and a completed task whose issue awaits review. A completed
// task whose issue is already ai/done needs no further work, and every other
// status stays display-only in the conversation.
func originChatResultNeedsHandoff(issue db.Issue, task db.AgentTaskQueue) bool {
	switch task.Status {
	case "failed", "cancelled":
		return true
	}
	if task.FailureReason.Valid && strings.TrimSpace(task.FailureReason.String) != "" {
		return true
	}
	if task.Error.Valid && strings.TrimSpace(task.Error.String) != "" {
		return true
	}
	if issue.Status == "blocked" {
		return true
	}
	return task.Status == "completed" && issue.Status == "in_review"
}

// originChatHandoffReason describes why the result was handed back, in the
// same words the card and the handoff record use.
func originChatHandoffReason(issue db.Issue, task db.AgentTaskQueue) string {
	switch {
	case task.Status == "failed":
		return "任务失败"
	case task.Status == "cancelled":
		return "任务已取消"
	case issue.Status == "blocked":
		return "issue 已阻塞"
	case task.Status == "completed" && issue.Status == "in_review":
		return "任务完成，issue 待审核"
	default:
		return "任务未成功结束"
	}
}

// handoffOriginChatResult wakes the origin chat agent on the sub-issue after a
// result that needs action, and leaves one visible handoff record.
//
// It deliberately does not open a chat turn: the member's conversation stays
// display-only, and the follow-up work happens on the issue. The enqueue is the
// same issue-dispatch path assignment and mention already use, so the run is
// attributed through the issue's provenance (the human who started the chat)
// rather than through a new authority.
//
// Idempotency has two layers:
//   - the handoff record (issue, source task) short-circuits a redelivery;
//   - the pending-task unique index coalesces a concurrent sibling enqueue.
//
// The returned bool reports whether the result was handed off. A false return
// means the card must stay display-only.
func (s *TaskService) handoffOriginChatResult(ctx context.Context, issue db.Issue, agent db.Agent, task db.AgentTaskQueue) (bool, error) {
	if !originChatResultNeedsHandoff(issue, task) {
		return false, nil
	}
	// A run performed by the origin agent itself needs no handoff: the agent
	// already owns the work, and waking it again would make every in_review
	// completion start another run on the same issue.
	if task.AgentID == agent.ID {
		return false, nil
	}

	_, err := s.Queries.GetTaskResultHandoffComment(ctx, db.GetTaskResultHandoffCommentParams{
		IssueID:      issue.ID,
		WorkspaceID:  issue.WorkspaceID,
		SourceTaskID: task.ID,
	})
	switch {
	case err == nil:
		// This delivery already woke the origin agent (or a redelivery of the
		// same run did). The card still reports the handoff.
		return true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("load task result handoff: %w", err)
	}

	key := IssueIdentifier(s.getIssuePrefix(issue.WorkspaceID), issue.Number)
	reason := originChatHandoffReason(issue, task)
	note := fmt.Sprintf("子任务 %s 的结果需要你处理（%s）。详见该 issue 的交接记录。", key, reason)
	_, enqueueErr := s.enqueueMentionTask(
		ctx, issue, agent.ID, pgtype.UUID{}, false, pgtype.UUID{}, false,
		note, pgtype.UUID{}, pgtype.UUID{}, OriginDerived,
	)
	if enqueueErr != nil {
		if !errors.Is(enqueueErr, ErrDuplicatePendingTask) {
			return false, fmt.Errorf("wake origin chat agent: %w", enqueueErr)
		}
		// Another trigger already queued this agent on the issue; the handoff
		// record still belongs here so the run can see why it was woken.
	}

	if err := s.ensureTaskResultHandoffComment(ctx, issue, agent, task, key, reason); err != nil {
		// The run is already queued, so the handoff happened; losing the record
		// must not turn the delivery into a failure. A redelivery retries the
		// record (the enqueue then coalesces).
		slog.Warn("task result handoff comment failed",
			"issue_id", util.UUIDToString(issue.ID),
			"task_id", util.UUIDToString(task.ID),
			"agent_id", util.UUIDToString(agent.ID),
			"error", err)
	}
	return true, nil
}

// ensureTaskResultHandoffComment writes the visible handoff record once per
// (issue, source task). The source task row is locked so two concurrent
// deliveries cannot both insert a record.
func (s *TaskService) ensureTaskResultHandoffComment(ctx context.Context, issue db.Issue, agent db.Agent, task db.AgentTaskQueue, key, reason string) error {
	var comment db.Comment
	created := false
	if err := s.runInTx(ctx, func(qtx *db.Queries) error {
		if _, err := qtx.GetAgentTaskForDelegatedFailureUpdate(ctx, task.ID); err != nil {
			return fmt.Errorf("lock source task: %w", err)
		}
		existing, err := qtx.GetTaskResultHandoffComment(ctx, db.GetTaskResultHandoffCommentParams{
			IssueID:      issue.ID,
			WorkspaceID:  issue.WorkspaceID,
			SourceTaskID: task.ID,
		})
		if err == nil {
			comment = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find task result handoff: %w", err)
		}
		createdComment, err := qtx.CreateComment(ctx, db.CreateCommentParams{
			ID:           dbid.NewV7(),
			IssueID:      issue.ID,
			WorkspaceID:  issue.WorkspaceID,
			AuthorType:   "system",
			AuthorID:     pgtype.UUID{Valid: true},
			Content:      taskResultHandoffContent(agent, task, key, reason),
			Type:         "system",
			SourceTaskID: task.ID,
		})
		if err != nil {
			return fmt.Errorf("create task result handoff: %w", err)
		}
		comment = createdComment.Comment()
		created = true
		return nil
	}); err != nil {
		return err
	}
	if !created || s.Bus == nil {
		return nil
	}
	s.Bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: util.UUIDToString(issue.WorkspaceID),
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"comment":      commentEventFields(comment),
			"issue_title":  issue.Title,
			"issue_status": issue.Status,
		},
	})
	return nil
}

func taskResultHandoffContent(agent db.Agent, task db.AgentTaskQueue, key, reason string) string {
	lines := []string{
		fmt.Sprintf("%s 子任务结果需要处理 · %s · %s", taskResultHandoffCommentMarker, key, reason),
		"",
		fmt.Sprintf("由「%s」执行的任务 %s 已结束（%s），结果已回传到创建该 issue 的会话。", safeCell(agent.Name), util.UUIDToString(task.ID), task.Status),
		"本 issue 的后续处理由创建它的会话智能体接手；该会话只展示结果卡片，不再回复。",
	}
	return strings.Join(lines, "\n")
}
