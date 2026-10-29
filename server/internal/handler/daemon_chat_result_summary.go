package handler

import (
	"context"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Bounds for the sub-issue result recap appended to a chat claim (LS-206).
// The cards are display-only in the conversation, so the next turn is the only
// place the agent can learn about them; the recap must stay small enough that
// it can never crowd out the member's own message.
const (
	chatTaskResultSummaryLimit      = 5
	chatTaskResultSummaryCardRunes  = 600
	chatTaskResultSummaryTotalRunes = 4000
	chatTaskResultSummaryHeading    = "上一轮之后到达的子任务结果（仅展示，处理记录在各自 issue）："
)

// taskResultSummaryForClaim returns the recap of result cards that arrived
// after this session's previous chat turn, oldest first. A read failure is
// logged and dropped: the member's own message must still reach the agent.
func (h *Handler) taskResultSummaryForClaim(ctx context.Context, task db.AgentTaskQueue, sessionID pgtype.UUID) string {
	if !sessionID.Valid {
		return ""
	}
	cards, err := h.Queries.ListTaskResultMessagesAfterPreviousChatTask(ctx, db.ListTaskResultMessagesAfterPreviousChatTaskParams{
		ChatSessionID: sessionID,
		CurrentTaskID: task.ID,
		MaxRows:       chatTaskResultSummaryLimit,
	})
	if err != nil {
		slog.Warn("chat claim: load task result summary failed",
			"task_id", util.UUIDToString(task.ID),
			"chat_session_id", util.UUIDToString(sessionID),
			"error", err)
		return ""
	}
	if len(cards) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cards))
	for i := len(cards) - 1; i >= 0; i-- {
		parts = append(parts, service.TaskResultCardSummary(cards[i].Content, chatTaskResultSummaryCardRunes))
	}
	block := chatTaskResultSummaryHeading + "\n" + strings.Join(parts, "\n")
	runes := []rune(block)
	if len(runes) > chatTaskResultSummaryTotalRunes {
		block = string(runes[:chatTaskResultSummaryTotalRunes-1]) + "…"
	}
	return block
}
