package handler

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Chat-session access by principal (Jartan fork).
//
// Upstream authorises every chat surface on the session's creator alone:
// `loadChatSessionForUser` admits when `chat_session.creator_id` equals the
// request's `X-User-ID`, and the list endpoints select `WHERE creator_id = $2`.
// The auth middleware stamps an `mat_` task token's `X-User-ID` with the token
// row's OWNING HUMAN, so every agent run in a single-owner workspace resolves to
// the same user id as the owner and as every other seat. The creator predicate
// therefore separates nobody from anybody: from inside an ordinary run, `GET
// /api/chat/sessions?status=all` returned every chat session in the workspace —
// the owner's private conversations included, with message content — and
// `GET /api/chat/sessions/{id}/messages` returned their full transcripts.
//
// The fix keys chat authorisation on the PRINCIPAL rather than on the user id.
// `resolveActor` already returns ("agent", id) for a task-token request, from a
// header the middleware forces off the token row and a client cannot forge, and
// `gateChatSessionForUser` already calls it — the agent identity is in hand at
// the moment the creator check runs.
//
// The rule for an agent principal is deny-by-default with two openings:
//
//   - the session's PINNED agent (`chat_session.agent_id`) — the conversation is
//     that agent's own, and its runs are handed the transcript anyway; and
//   - the session's CREATING agent (`chat_session.creator_agent_id`, migration
//     901) — an agent that opened a session to talk to another agent has to be
//     able to read the reply.
//
// Everything else is unreachable, including every session a human created with
// another agent. Member requests are untouched: the creator check remains the
// whole rule for them.
//
// This lives in its own file so the fork's diff against upstream's `chat.go`
// stays to the call sites, and so a rebase that moves the handlers does not move
// the rule.

// chatActorScope is the calling principal's chat reachability rule, resolved
// once per request. The zero value is a MEMBER scope, which allows everything
// the upstream creator check already allowed — so a call site that forgets to
// build a scope fails open to upstream behaviour rather than locking members
// out of their own chats. Agent scopes are built only by newChatActorScope.
type chatActorScope struct {
	isAgent bool
	agentID string
}

// newChatActorScope builds the scope from an already-resolved actor, so a call
// site that has run resolveActor for its own gate does not run it twice.
func newChatActorScope(actorType, actorID string) chatActorScope {
	if actorType != "agent" {
		return chatActorScope{}
	}
	return chatActorScope{isAgent: true, agentID: actorID}
}

// chatActorScopeFor resolves the actor and builds its scope, for call sites that
// do not already need the actor for something else.
func (h *Handler) chatActorScopeFor(r *http.Request, userID, workspaceID string) chatActorScope {
	return newChatActorScope(h.resolveActor(r, userID, workspaceID))
}

// allows reports whether this principal may reach a session with the given
// pinned agent and creating agent. Members are allowed (their creator check
// already ran); an agent principal is allowed only on the two openings above.
// An agent scope carrying no agent id fails closed.
func (s chatActorScope) allows(sessionAgentID, sessionCreatorAgentID pgtype.UUID) bool {
	if !s.isAgent {
		return true
	}
	if s.agentID == "" {
		return false
	}
	if uuidToString(sessionAgentID) == s.agentID {
		return true
	}
	if sessionCreatorAgentID.Valid && uuidToString(sessionCreatorAgentID) == s.agentID {
		return true
	}
	return false
}

// allowsSession is allows() for a loaded session row.
func (s chatActorScope) allowsSession(session db.ChatSession) bool {
	return s.allows(session.AgentID, session.CreatorAgentID)
}

// creatorAgentID is the value to stamp on a session this principal is creating:
// the agent's own id for an agent principal, NULL for a human. Written from the
// request's task token, never from the request body.
func (s chatActorScope) creatorAgentID() pgtype.UUID {
	if !s.isAgent {
		return pgtype.UUID{}
	}
	return optionalUUID(s.agentID)
}

// optionalUUID parses an id that is legitimately absent — a header a member
// request does not carry — into an invalid (SQL NULL) UUID. The handler
// package's parseUUID panics on anything unparseable, which is correct for a
// value the caller has already validated and wrong for one that is simply not
// there.
func optionalUUID(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	parsed, err := util.ParseUUID(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return parsed
}

// denyChatSessionForActor writes the refusal for a session an agent principal
// may not reach. It is a 404 rather than a 403 so the surface stays
// enumeration-safe: probing session ids from a run tells the caller nothing
// about which sessions exist.
func denyChatSessionForActor(w http.ResponseWriter, scope chatActorScope, session db.ChatSession) {
	slog.Debug("chat session refused: not reachable from this agent principal",
		"agent_id", scope.agentID,
		"chat_session_id", uuidToString(session.ID),
		"session_agent_id", uuidToString(session.AgentID),
	)
	writeError(w, http.StatusNotFound, "chat session not found")
}
