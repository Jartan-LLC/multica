package service

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// agentChatSendFacts gathers the facts attribution.AgentChatSend classifies for
// a chat message sent from an agent run (Jartan fork, SEC-2026-0079).
//
// The DB read stays here, out of the attribution package, so the classification
// rules remain pure and unit-testable without a database — the same split every
// other Classify* path uses.
//
// A sending task that cannot be read (no task id on the request, a row that has
// since been deleted) yields facts with no human in them, which
// AgentChatSend turns into an explicit unattributed result rather than a guess.
// The workspace's own attribution-fallback policy then decides whether that
// degrades to the agent owner or fails the send closed — unchanged behaviour for
// an unattributable enqueue.
func (s *TaskService) agentChatSendFacts(ctx context.Context, session db.ChatSession, senderTaskID pgtype.UUID) attribution.AgentChatSendFacts {
	facts := attribution.AgentChatSendFacts{
		ChatSessionID: session.ID,
		SendingTaskID: senderTaskID,
	}
	if !senderTaskID.Valid || s == nil || s.Queries == nil {
		return facts
	}
	sender, err := s.Queries.GetAgentTask(ctx, senderTaskID)
	if err != nil {
		slog.Debug("agent chat send: sending task not readable, attributing without it",
			"chat_session_id", util.UUIDToString(session.ID),
			"sending_task_id", util.UUIDToString(senderTaskID),
			"error", err)
		return facts
	}
	facts.SendingOriginator = sender.OriginatorUserID
	facts.SendingAccountable = sender.AccountableUserID
	return facts
}
