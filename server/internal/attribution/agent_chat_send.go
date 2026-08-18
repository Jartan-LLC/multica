package attribution

import "github.com/jackc/pgx/v5/pgtype"

// Agent-sent chat messages (Jartan fork).
//
// Upstream stamps every direct chat send with DirectHumanRun — "a member's own
// action enqueued the run", source direct_human, which Precise() classifies as
// compliance-grade. That is true of a web send and false of a send from an agent
// run: the server resolves the sending principal as an agent on the way in, then
// attributes the run to the token's owning human anyway.
//
// A send from one agent to another is the case SourceDelegation already models
// — "an agent running on behalf of a human caused the enqueue" — with the
// accountable human COPIED from the sending task rather than chained, so
// delegation cycles stay harmless (MUL-4302 §3.2). This constructor is the
// chat-shaped analogue of ClassifyDirect's quick_create / agent_create branch,
// and it exists here rather than as a hand-built Result literal so the
// accountability invariant is finalised the same way as every other path.

// AgentChatSendFacts are the already-fetched facts about an agent-sent chat
// message, gathered by the caller so classification stays pure.
type AgentChatSendFacts struct {
	// ChatSessionID is the evidence ref — the session the message landed in,
	// matching EvidenceChat on the member path.
	ChatSessionID pgtype.UUID

	// SendingTaskID is the run that sent the message, resolved from its own
	// `mat_` task token. Recorded as delegated_from_task_id, which is what makes
	// the hop traceable back to a real run rather than to a name.
	SendingTaskID pgtype.UUID

	// SendingOriginator / SendingAccountable are the sending task's
	// originator_user_id and accountable_user_id. Copied, never chained.
	SendingOriginator  pgtype.UUID
	SendingAccountable pgtype.UUID
}

// AgentChatSend resolves attribution for a chat message sent from an agent run.
//
// The waterfall mirrors ClassifyDirect's delegation branch:
//   - the sending task has an authorizing human → delegation, that human is
//     originator and accountable;
//   - the sending task is autopilot-rooted (no originator, accountable set) →
//     delegation, accountable copied down, originator stays NULL;
//   - the sending task resolved no human at all → unattributed, with real
//     evidence, so the row is classified rather than a NULL-source bypass.
//
// In every branch the run is NOT direct_human, and the sender is an agent.
func AgentChatSend(f AgentChatSendFacts) Result {
	r := Result{
		DelegatedFromTaskID: f.SendingTaskID,
		EvidenceKind:        EvidenceChat,
		EvidenceRefID:       f.ChatSessionID,
	}
	switch {
	case f.SendingOriginator.Valid:
		r.UserID = f.SendingOriginator
		r.Source = SourceDelegation
	case f.SendingAccountable.Valid:
		r.AccountableUserID = f.SendingAccountable
		r.Source = SourceDelegation
	default:
		r.Source = SourceUnattributed
	}
	return finalizeAttribution(r)
}
