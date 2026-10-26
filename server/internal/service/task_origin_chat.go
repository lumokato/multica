package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

const maxOriginChatResultRunes = 8000

// deliverIssueResultToOriginChat feeds a completed issue task back into the
// active direct chat that created it. This is deliberately outside the issue
// completion transaction: a chat enqueue must not hold the issue/task locks or
// make completion fail when the originating chat has since been archived.
func (s *TaskService) deliverIssueResultToOriginChat(ctx context.Context, issueTask db.AgentTaskQueue, result []byte) {
	if s == nil || s.Queries == nil || !issueTask.IssueID.Valid {
		return
	}
	issue, err := s.Queries.GetIssue(ctx, issueTask.IssueID)
	if err != nil {
		if !isNoRows(err) {
			slog.Warn("deliver issue result to origin chat: load issue failed", "task_id", util.UUIDToString(issueTask.ID), "issue_id", util.UUIDToString(issueTask.IssueID), "error", err)
		}
		return
	}
	if !issue.OriginType.Valid || !issue.OriginID.Valid || (issue.OriginType.String != "agent_create" && issue.OriginType.String != "quick_create") {
		return
	}
	originTask, err := s.Queries.GetAgentTask(ctx, issue.OriginID)
	if err != nil {
		if !isNoRows(err) {
			slog.Warn("deliver issue result to origin chat: load origin task failed", "task_id", util.UUIDToString(issueTask.ID), "origin_task_id", util.UUIDToString(issue.OriginID), "error", err)
		}
		return
	}
	if !originTask.ChatSessionID.Valid {
		return
	}
	session, err := s.Queries.GetChatSession(ctx, originTask.ChatSessionID)
	if err != nil {
		if !isNoRows(err) {
			slog.Warn("deliver issue result to origin chat: load session failed", "task_id", util.UUIDToString(issueTask.ID), "chat_session_id", util.UUIDToString(originTask.ChatSessionID), "error", err)
		}
		return
	}
	if session.Status != "active" {
		return
	}
	agent, err := s.Queries.GetAgent(ctx, session.AgentID)
	if err != nil {
		slog.Warn("deliver issue result to origin chat: load agent failed", "task_id", util.UUIDToString(issueTask.ID), "agent_id", util.UUIDToString(session.AgentID), "error", err)
		return
	}

	prefix := s.getIssuePrefix(issue.WorkspaceID)
	key := IssueIdentifier(prefix, issue.Number)
	body := originChatResultBody(result)
	content := fmt.Sprintf("[TASK_RESULT]\n%s — %s\nstatus: %s\ntask: %s\n\n---\n%s\n\n请基于以上结果安排下一步，或验收通过。", key, issue.Title, issue.Status, util.UUIDToString(issueTask.ID), body)
	if _, err := s.SendDirectChatMessage(ctx, session, agent, session.CreatorID, content, nil, "member", session.CreatorID); err != nil {
		slog.Warn("deliver issue result to origin chat: enqueue failed", "task_id", util.UUIDToString(issueTask.ID), "issue_id", util.UUIDToString(issue.ID), "chat_session_id", util.UUIDToString(session.ID), "error", err)
	}
}

func originChatResultBody(result []byte) string {
	var payload protocol.TaskCompletedPayload
	if err := json.Unmarshal(result, &payload); err == nil && payload.Output != "" {
		result = []byte(payload.Output)
	}
	body := strings.TrimSpace(redact.Text(string(result)))
	runes := []rune(body)
	if len(runes) > maxOriginChatResultRunes {
		body = string(runes[:maxOriginChatResultRunes-1]) + "…"
	}
	return body
}

func isNoRows(err error) bool {
	return err != nil && (err == pgx.ErrNoRows || strings.Contains(err.Error(), pgx.ErrNoRows.Error()))
}
