package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The owner-facing surface of the agent-definition write gate (Jartan fork):
// read and replace the workspace's allow-list of agents that
// may mutate agent definitions. The gate itself is
// internal/handler/agent_definition_gate.go.
//
// Both routes are ADDITIVE — new paths, no change to any existing request or
// response shape — so an unmodified upstream CLI keeps working against this
// server unchanged — a binding constraint on this fork.
//
// The write is human-workspace-owner-only, and that is what keeps the control
// from being circular: if a listed agent could edit the list, one compromised
// seat would widen the breach to every seat, and the gate would bound nothing.
// Enforced twice on purpose — RequireHumanActor on the route (rejects mat_ task
// tokens and mcn_ cloud PATs) and the owner-role check here — following the
// backstop convention actor_guards.go documents: a route-group edit that drops
// the middleware must not silently open the list.

// agentDefinitionWritersActivityUpdated is the activity_log action for a
// change to the allow-list itself. The list decides which agents hold the
// privilege, so a change to it is more sensitive than any single write it
// permits, and it is written inside the same transaction as the change.
const agentDefinitionWritersActivityUpdated = "agent_definition_writers_updated"

// AgentDefinitionWriterResponse is one allow-list entry.
type AgentDefinitionWriterResponse struct {
	AgentID   string  `json:"agent_id"`
	AgentName string  `json:"agent_name,omitempty"`
	CreatedBy *string `json:"created_by,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// SetAgentDefinitionWritersRequest replaces the whole list. A wholesale replace
// (rather than add/remove verbs) matches the invocation-target write model and
// makes the resulting state independent of what the caller believed was there.
// An empty or omitted array clears the list, which is the fail-closed state:
// no agent may mutate any agent definition.
type SetAgentDefinitionWritersRequest struct {
	AgentIDs []string `json:"agent_ids"`
}

// ListAgentDefinitionWriters serves the workspace's allow-list. Readable by any
// workspace member: the list names privilege, it does not carry a secret, and
// an operator debugging a 403 needs to see it. The workspace-membership
// middleware on the route group is the only gate.
func (h *Handler) ListAgentDefinitionWriters(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found"); !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	rows, err := h.Queries.ListAgentDefinitionWriters(r.Context(), wsUUID)
	if err != nil {
		slog.Warn("list agent-definition writers failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list agent-definition writers")
		return
	}

	// Names are a convenience for the operator reading the list; a row whose
	// agent no longer exists still appears, with an empty name, rather than
	// being hidden — a stale grant should be visible so it can be removed.
	resp := make([]AgentDefinitionWriterResponse, 0, len(rows))
	for _, row := range rows {
		entry := AgentDefinitionWriterResponse{
			AgentID:   uuidToString(row.AgentID),
			CreatedBy: uuidToPtr(row.CreatedBy),
			CreatedAt: timestampToString(row.CreatedAt),
		}
		if agent, err := h.Queries.GetAgent(r.Context(), row.AgentID); err == nil {
			entry.AgentName = agent.Name
		}
		resp = append(resp, entry)
	}
	writeJSON(w, http.StatusOK, resp)
}

// SetAgentDefinitionWriters replaces the allow-list. Human workspace owner
// only. Every id must name an agent that exists in this workspace: a typo
// would otherwise be accepted as a grant that silently does nothing, and the
// operator would believe People Ops was working when it was not.
func (h *Handler) SetAgentDefinitionWriters(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)

	// Backstop for the route middleware (see file comment). A machine
	// credential never edits this list, whoever owns it.
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "only a human workspace owner can change the agent-definition writer allow-list")
		return
	}
	member, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	var req SetAgentDefinitionWritersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	seen := make(map[string]struct{}, len(req.AgentIDs))
	agentUUIDs := make([]pgtype.UUID, 0, len(req.AgentIDs))
	for _, raw := range req.AgentIDs {
		agentUUID, err := util.ParseUUID(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "agent_ids must be agent UUIDs")
			return
		}
		if _, dup := seen[raw]; dup {
			continue
		}
		agent, err := h.Queries.GetAgent(r.Context(), agentUUID)
		if err != nil || uuidToString(agent.WorkspaceID) != workspaceID {
			writeError(w, http.StatusBadRequest, "agent_ids must name agents in this workspace")
			return
		}
		seen[raw] = struct{}{}
		agentUUIDs = append(agentUUIDs, agentUUID)
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		slog.Error("set agent-definition writers: begin tx failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update the agent-definition writer allow-list")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	// Replace wholesale inside one transaction: a partially applied list is a
	// partially applied access-control decision.
	if err := qtx.DeleteAgentDefinitionWriters(r.Context(), wsUUID); err != nil {
		slog.Warn("clear agent-definition writers failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update the agent-definition writer allow-list")
		return
	}
	for _, agentUUID := range agentUUIDs {
		if err := qtx.CreateAgentDefinitionWriter(r.Context(), db.CreateAgentDefinitionWriterParams{
			WorkspaceID: wsUUID,
			AgentID:     agentUUID,
			CreatedBy:   member.UserID,
		}); err != nil {
			slog.Warn("add agent-definition writer failed",
				append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agentUUID))...)
			writeError(w, http.StatusInternalServerError, "failed to update the agent-definition writer allow-list")
			return
		}
	}

	ids := make([]string, 0, len(agentUUIDs))
	for _, agentUUID := range agentUUIDs {
		ids = append(ids, uuidToString(agentUUID))
	}
	details, _ := json.Marshal(map[string]any{
		"agent_ids": ids,
		"count":     len(ids),
	})
	if _, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: wsUUID,
		IssueID:     pgtype.UUID{},
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     member.UserID,
		Action:      agentDefinitionWritersActivityUpdated,
		Details:     details,
	}); err != nil {
		slog.Error("agent_definition_writers_updated audit write failed; rolling back",
			append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "audit log write failed; allow-list change rolled back")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("set agent-definition writers: tx commit failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to update the agent-definition writer allow-list")
		return
	}

	slog.Info("agent-definition writer allow-list updated",
		append(logger.RequestAttrs(r), "workspace_id", workspaceID, "count", len(ids))...)
	h.ListAgentDefinitionWriters(w, r)
}
