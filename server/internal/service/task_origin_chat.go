package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

const maxOriginChatResultRunes = 8000

// originChatResultNamespace scopes the deterministic ids minted for result
// cards, so a card's identity can never collide with an id from another
// origin.
var originChatResultNamespace = uuid.NewSHA1(uuid.NameSpaceOID, []byte("multica/origin-chat-result"))

// DeliverIssueResultToOriginChat republishes a terminal/status result for a
// task after an agent or system-driven issue update. It is intentionally
// best-effort and is a public wrapper for handlers that already know the
// source task.
func (s *TaskService) DeliverIssueResultToOriginChat(ctx context.Context, task db.AgentTaskQueue) {
	s.deliverIssueResultToOriginChat(ctx, task, task.Result, "", "")
}

// deliverIssueResultToOriginChat feeds a terminal issue task back into the
// direct chat that created it. The lookup and send deliberately happen after
// the task transaction has committed: the result message must never make a
// task transition fail.
func (s *TaskService) deliverIssueResultToOriginChat(ctx context.Context, issueTask db.AgentTaskQueue, result []byte, failureReason, errorText string) {
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

	payload := s.loadOriginChatPayload(ctx, issueTask, issue, originTask, agent, result, failureReason, errorText)
	// LS-206: a result that needs action wakes the origin chat agent on the
	// sub-issue. The card itself never opens a chat turn; it only reports
	// whether the result was handed off.
	payload.Processing = "仅展示"
	if handled, handoffErr := s.handoffOriginChatResult(ctx, issue, agent, issueTask); handoffErr != nil {
		slog.Warn("deliver issue result to origin chat: handoff failed", "task_id", util.UUIDToString(issueTask.ID), "issue_id", util.UUIDToString(issue.ID), "agent_id", util.UUIDToString(agent.ID), "error", handoffErr)
	} else if handled {
		payload.Processing = "已交主智能体处理"
	}
	prefix := s.getIssuePrefix(issue.WorkspaceID)
	content := formatOriginChatResultWithPrefix(prefix, payload)
	message, created, err := s.insertOriginChatResultMessage(ctx, session, issueTask, content)
	if err != nil {
		slog.Warn("deliver issue result to origin chat: persist failed", "task_id", util.UUIDToString(issueTask.ID), "issue_id", util.UUIDToString(issue.ID), "chat_session_id", util.UUIDToString(session.ID), "error", err)
		return
	}
	if !created {
		// This run already published its card (terminal transition, then an
		// issue update — or a redelivered webhook). Dropping the repeat is what
		// keeps one run to one card and one chat turn.
		return
	}
	s.publishOriginChatResultMessage(session, message)
}

// insertOriginChatResultMessage stores the result card as a server-authored
// assistant message that owns no task.
//
// It deliberately does NOT go through SendDirectChatMessage: that path opens a
// chat turn, and every card would then enqueue another agent run (64 cards →
// 64 runs in LS-173). A task-less assistant row is display-only — the runtime's
// input batch is loaded by task_id, so nothing here can become model input,
// and an unowned role='user' row would instead be adopted by the next turn's
// batch and replayed to the agent as if the member had typed it.
//
// The primary key is derived from (chat session, delivering task), so a second
// delivery of the same run collides on the primary key and is dropped instead
// of appending a duplicate card.
func (s *TaskService) insertOriginChatResultMessage(ctx context.Context, session db.ChatSession, task db.AgentTaskQueue, content string) (db.ChatMessage, bool, error) {
	message, err := s.Queries.CreateChatMessage(ctx, db.CreateChatMessageParams{
		ID:            originChatResultMessageID(session.ID, task.ID),
		ChatSessionID: session.ID,
		Role:          "assistant",
		Content:       content,
		MessageKind:   pgtype.Text{String: protocol.ChatMessageKindTaskResult, Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			return db.ChatMessage{}, false, nil
		}
		return db.ChatMessage{}, false, err
	}
	return message, true, nil
}

// originChatResultMessageID derives the card's id from the pair that makes one
// delivery unique. Name-based (UUIDv5) rather than random: the id is the
// idempotency key, so the same run must always derive the same value.
func originChatResultMessageID(chatSessionID, taskID pgtype.UUID) pgtype.UUID {
	name := util.UUIDToString(chatSessionID) + ":" + util.UUIDToString(taskID)
	return pgtype.UUID{Bytes: uuid.NewSHA1(originChatResultNamespace, []byte(name)), Valid: true}
}

// publishOriginChatResultMessage tells other open clients of the session that
// the card landed. Without it the row would only appear on the next refetch:
// this path publishes no task event to drive one.
func (s *TaskService) publishOriginChatResultMessage(session db.ChatSession, message db.ChatMessage) {
	if s.Bus == nil || !session.WorkspaceID.Valid || !message.CreatedAt.Valid {
		return
	}
	s.Bus.Publish(events.Event{
		Type:          protocol.EventChatMessage,
		WorkspaceID:   util.UUIDToString(session.WorkspaceID),
		ActorType:     "system",
		ChatSessionID: util.UUIDToString(session.ID),
		Payload: protocol.ChatMessagePayload{
			ChatSessionID: util.UUIDToString(session.ID),
			MessageID:     util.UUIDToString(message.ID),
			Role:          message.Role,
			Content:       message.Content,
			CreatedAt:     message.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		},
	})
}

type originChatPayload struct {
	Task       db.AgentTaskQueue
	Issue      db.Issue
	OriginTask db.AgentTaskQueue
	Agent      db.Agent
	Usage      []db.TaskUsage
	Messages   []db.TaskMessage
	Output     string
	PRURL      string
	Failure    string
	Error      string
	// Processing is the card's "what happened to this result" line: handed to
	// the origin agent on the sub-issue, or display-only in the conversation.
	Processing string
}

func (s *TaskService) loadOriginChatPayload(ctx context.Context, task db.AgentTaskQueue, issue db.Issue, originTask db.AgentTaskQueue, chatAgent db.Agent, result []byte, failureReason, errorText string) originChatPayload {
	payload := newOriginChatPayload(task, issue, originTask, chatAgent, result, failureReason, errorText)
	if executionAgent, err := s.Queries.GetAgent(ctx, task.AgentID); err == nil {
		payload.Agent = executionAgent
	} else {
		slog.Warn("deliver issue result to origin chat: load execution agent failed", "task_id", util.UUIDToString(task.ID), "agent_id", util.UUIDToString(task.AgentID), "error", err)
	}
	var completed protocol.TaskCompletedPayload
	if err := json.Unmarshal(result, &completed); err == nil {
		payload.Output = completed.Output
		payload.PRURL = completed.PRURL
	}
	if rows, err := s.Queries.ListTaskMessages(ctx, task.ID); err == nil {
		payload.Messages = rows
	} else {
		slog.Warn("deliver issue result to origin chat: load task messages failed", "task_id", util.UUIDToString(task.ID), "error", err)
	}
	lastComment := ""
	if comment, err := s.Queries.GetLastAgentCommentForTask(ctx, db.GetLastAgentCommentForTaskParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, SourceTaskID: task.ID}); err == nil {
		lastComment = comment.Content
	}
	payload.Output = chooseOriginChatOutput(payload.Output, payload.Messages, lastComment, IssueIdentifier(s.getIssuePrefix(issue.WorkspaceID), issue.Number))
	if rows, err := s.Queries.GetTaskUsage(ctx, task.ID); err == nil {
		payload.Usage = rows
	} else {
		slog.Warn("deliver issue result to origin chat: load usage failed", "task_id", util.UUIDToString(task.ID), "error", err)
	}
	return payload
}

func chooseOriginChatOutput(output string, messages []db.TaskMessage, lastComment, issueKey string) string {
	if strings.TrimSpace(output) != "" {
		return output
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Type == "text" && strings.TrimSpace(messages[i].Content.String) != "" {
			return messages[i].Content.String
		}
	}
	if strings.TrimSpace(lastComment) != "" {
		return lastComment
	}
	return fmt.Sprintf("本次 run 无文本输出，见 issue %s 评论", issueKey)
}

func newOriginChatPayload(task db.AgentTaskQueue, issue db.Issue, originTask db.AgentTaskQueue, agent db.Agent, result []byte, failureReason, errorText string) originChatPayload {
	payload := originChatPayload{Task: task, Issue: issue, OriginTask: originTask, Agent: agent, Failure: failureReason, Error: errorText}
	var completed protocol.TaskCompletedPayload
	if err := json.Unmarshal(result, &completed); err == nil {
		payload.Output, payload.PRURL = completed.Output, completed.PRURL
	} else if failureReason == "" {
		payload.Output = string(result)
	}
	return payload
}

func formatOriginChatResult(p originChatPayload) string {
	return formatOriginChatResultWithPrefix("", p)
}

func formatOriginChatResultWithPrefix(prefix string, p originChatPayload) string {
	key := IssueIdentifier(prefix, p.Issue.Number)
	if prefix == "" {
		key = fmt.Sprintf("#%d", p.Issue.Number)
	}
	status := p.Issue.Status
	if status == "in_review" {
		status += "（待负责人审核）"
	}
	rows := []string{
		fmt.Sprintf("⟦任务结果⟧ 📋 子任务结果 · %s · %s · 由「%s」产出（不是本会话回复）", key, status, safeCell(p.Agent.Name)),
		fmt.Sprintf("处理状态：%s", p.Processing),
		"",
		"| 项 | 值 |",
		"|---|---|",
		fmt.Sprintf("| issue | %s · %s · %s |", key, status, util.UUIDToString(p.Issue.ID)),
		fmt.Sprintf("| run | %s · attempt %d · %s |", p.Task.Status, p.Task.Attempt, util.UUIDToString(p.Task.ID)),
		fmt.Sprintf("| chat | %s |", util.UUIDToString(p.OriginTask.ChatSessionID)),
		fmt.Sprintf("| agent | %s · %s |", safeCell(p.Agent.Name), util.UUIDToString(p.Agent.ID)),
	}
	if model := formatOriginUsage(p.Usage); model != "" {
		rows = append(rows, "| model | "+model+" |")
	}
	rows = append(rows, "| time | "+formatOriginTime(p.Task, p.Messages)+" |")
	rows = append(rows, "| trigger | "+formatOriginTrigger(p.Task)+" |")
	if p.Task.DelegatedFromTaskID.Valid {
		rows = append(rows, fmt.Sprintf("| from | %s |", util.UUIDToString(p.Task.DelegatedFromTaskID)))
	}
	if p.Task.RetryOfTaskID.Valid {
		rows = append(rows, fmt.Sprintf("| retry_of | %s |", util.UUIDToString(p.Task.RetryOfTaskID)))
	}
	if pr := formatOriginPR(p.PRURL); pr != "" {
		rows = append(rows, "| pr | "+pr+" |")
	}
	if p.Failure != "" {
		rows = append(rows, "| failure_reason | "+safeCell(p.Failure)+" |")
	}
	if p.Error != "" {
		rows = append(rows, "| error | "+safeCell(originChatResultBody([]byte(p.Error)))+" |")
	}
	body := originChatResultBody([]byte(p.Output))
	return strings.Join(rows, "\n") + "\n\n**执行体输出**\n\n" + body + "\n⟦任务结果结束⟧"
}

func formatOriginUsage(rows []db.TaskUsage) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%s · %s · in %d / out %d / cache_read %d / cache_write %d", safeCell(row.Model), safeCell(row.Provider), row.InputTokens, row.OutputTokens, row.CacheReadTokens, row.CacheWriteTokens))
	}
	return strings.Join(parts, "; ")
}

func formatOriginTime(task db.AgentTaskQueue, messages []db.TaskMessage) string {
	terminal := isTerminalOriginTask(task)
	// Never write a bare "?" for a missing timestamp: a queued run has no
	// execution segment yet, and a run cancelled before dispatch never gets
	// one, so both are stated instead of guessed.
	queued := "—"
	if task.CreatedAt.Valid && task.StartedAt.Valid {
		queued = formatOriginDuration(task.StartedAt.Time.Sub(task.CreatedAt.Time))
	} else if terminal && task.CreatedAt.Valid && task.CompletedAt.Valid {
		queued = formatOriginDuration(task.CompletedAt.Time.Sub(task.CreatedAt.Time))
	}
	out := "queued " + queued
	if !task.StartedAt.Valid {
		if terminal {
			out += " · 未启动"
		}
		return out
	}
	if !terminal {
		return out
	}
	exec := "—"
	if task.CompletedAt.Valid {
		exec = formatOriginDuration(task.CompletedAt.Time.Sub(task.StartedAt.Time))
	}
	out += " · exec " + exec
	if len(messages) == 0 {
		return out
	}
	timing := deriveOriginMessageTiming(messages)
	if timing.turns > 0 {
		out += fmt.Sprintf(" · turns %d · model≈%s（中位 %s，派生）", timing.turns, formatOriginDuration(timing.modelTotal), formatOriginDuration(timing.modelMedian))
	}
	if timing.toolsTotal > 0 {
		out += " · tools≈" + formatOriginDuration(timing.toolsTotal) + "（最长 " + formatOriginDuration(timing.toolsMax) + "，派生）"
	}
	return out
}

func formatOriginDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int64(d / time.Second)
	if seconds < 1 {
		return "0s"
	}
	if seconds >= 3600 {
		return fmt.Sprintf("%dh%02dm%02ds", seconds/3600, (seconds%3600)/60, seconds%60)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}

type originMessageTiming struct {
	turns       int
	modelTotal  time.Duration
	modelMedian time.Duration
	toolsTotal  time.Duration
	toolsMax    time.Duration
}

func deriveOriginMessageTiming(messages []db.TaskMessage) originMessageTiming {
	var timing originMessageTiming
	var modelDurations []time.Duration
	uses := make(map[string]time.Time)
	for _, message := range messages {
		if !message.CreatedAt.Valid {
			continue
		}
		t := message.CreatedAt.Time
		switch message.Type {
		case "tool_use":
			if message.CallID.Valid {
				uses[message.CallID.String] = t
			}
		case "tool_result":
			if message.CallID.Valid {
				if start, ok := uses[message.CallID.String]; ok {
					duration := t.Sub(start)
					timing.toolsTotal += duration
					if duration > timing.toolsMax {
						timing.toolsMax = duration
					}
					delete(uses, message.CallID.String)
				}
			}
		default:
			timing.turns++
		}
	}
	for i := 1; i < len(messages); i++ {
		if messages[i-1].CreatedAt.Valid && messages[i].CreatedAt.Valid {
			duration := messages[i].CreatedAt.Time.Sub(messages[i-1].CreatedAt.Time)
			if duration >= time.Second {
				timing.modelTotal += duration
				modelDurations = append(modelDurations, duration)
			}
		}
	}
	if len(modelDurations) > 0 {
		sort.Slice(modelDurations, func(i, j int) bool { return modelDurations[i] < modelDurations[j] })
		timing.modelMedian = modelDurations[len(modelDurations)/2]
	}
	return timing
}

func formatOriginTrigger(task db.AgentTaskQueue) string {
	kind := "run"
	if task.OriginatorSource.Valid && task.OriginatorSource.String == "delegation" {
		kind = "delegation"
	} else if task.TriggerEvidenceKind.Valid && task.TriggerEvidenceKind.String != "" {
		kind = task.TriggerEvidenceKind.String
	}
	if kind == "issue_assignment" || (kind == "delegation" && task.TriggerEvidenceRefID.Valid && task.IssueID.Valid && task.TriggerEvidenceRefID == task.IssueID) {
		return "assignment"
	}
	if task.TriggerEvidenceRefID.Valid {
		return kind + " · " + util.UUIDToString(task.TriggerEvidenceRefID)
	}
	return kind
}

func isTerminalOriginTask(task db.AgentTaskQueue) bool {
	switch task.Status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func formatOriginPR(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(parts[i]); err == nil && n > 0 && i >= 1 {
			if i >= 2 && (parts[i-1] == "pull" || parts[i-1] == "merge_requests") {
				return parts[i-2] + " #" + strconv.Itoa(n)
			}
			return parts[i-1] + " #" + strconv.Itoa(n)
		}
	}
	return ""
}

func safeCell(value string) string {
	value = redactOriginChatURLs(value)
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func originChatResultBody(result []byte) string {
	body := strings.TrimSpace(redact.Text(string(result)))
	body = redactOriginChatLocalPaths(body)
	runes := []rune(body)
	if len(runes) > maxOriginChatResultRunes {
		body = string(runes[:maxOriginChatResultRunes-1]) + "…"
	}
	return body
}

func redactOriginChatLocalPaths(body string) string {
	for {
		start := strings.Index(body, "/Users/")
		if start < 0 {
			return body
		}
		end := start
		for end < len(body) && !strings.ContainsRune(" \t\r\n<>[]()'\"", rune(body[end])) {
			end++
		}
		body = body[:start] + "[本机路径已省略]" + body[end:]
	}
}

func redactOriginChatURLs(body string) string {
	for {
		start := strings.Index(strings.ToLower(body), "http://")
		secure := strings.Index(strings.ToLower(body), "https://")
		if start < 0 || (secure >= 0 && secure < start) {
			start = secure
		}
		if start < 0 {
			return body
		}
		end := start
		for end < len(body) && !strings.ContainsRune(" \t\r\n<>[]()", rune(body[end])) {
			end++
		}
		body = body[:start] + "[链接已省略]" + body[end:]
	}
}

func isNoRows(err error) bool {
	return err != nil && (err == pgx.ErrNoRows || strings.Contains(err.Error(), pgx.ErrNoRows.Error()))
}

// TaskResultCardSummary renders one delivered result card as the compact text
// appended to the origin agent's next chat prompt (LS-206). The card is the
// only durable carrier of the sub-task identity, so the summary reuses its
// header line (issue key, status, producing agent) and its output body instead
// of re-reading the issue.
func TaskResultCardSummary(content string, maxRunes int) string {
	header, body, _ := strings.Cut(content, "\n")
	header = strings.TrimSpace(header)
	header = strings.TrimSpace(strings.TrimPrefix(header, "⟦任务结果⟧"))
	header = strings.TrimSpace(strings.TrimPrefix(header, "📋"))
	if header == "" {
		header = "子任务结果"
	}
	if idx := strings.Index(content, "**执行体输出**"); idx >= 0 {
		body = strings.TrimSpace(content[idx+len("**执行体输出**"):])
		body = strings.TrimSpace(strings.TrimSuffix(body, "⟦任务结果结束⟧"))
	}
	if body == "" {
		return header
	}
	runes := []rune(body)
	if maxRunes > 0 && len(runes) > maxRunes {
		body = string(runes[:maxRunes-1]) + "…"
	}
	return header + "\n" + body
}
