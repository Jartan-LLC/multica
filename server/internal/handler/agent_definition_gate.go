package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Agent-definition write gate (Jartan fork).
//
// The finding: `canManageAgent` admits "the agent's owner OR a workspace
// owner/admin" and never asks WHICH KIND of principal is calling. An agent run
// authenticates with a mat_ task token whose X-User-ID is stamped to its
// OWNING human (middleware/auth.go), so on a workspace where one human owns
// every agent, every agent seat clears that check for every other agent. One
// turn of influence over any agent — an issue body, an attachment, a repo a
// run reads — converts into a permanent rewrite of another agent's
// instructions, model, MCP config or runtime, with no approval, no diff and no
// revision history.
//
// The fix is not a new access model. The platform already draws the exact
// distinction needed, server-side and unspoofably: `resolveActor` returns
// ("agent", id) only for a request carrying a server-validated task token, and
// two surfaces already reject that principal outright before any role check —
// `authorizeAgentEnv` (agent_env.go) and `requireAgentMcpWriter`
// (workspace_mcp_api.go). This gate applies that same predicate to the rest of
// the agent-record mutation surface, with one addition: an owner-configured
// allow-list, so the workforce function's standing authority to
// hire, tune and retire agents survives the gate.
//
// Shape precedents, both upstream:
//   - #6905 (MUL-6126) — a role-based path stripped down to an identity
//     predicate, enforced in the API rather than only the client.
//   - #6738 (MUL-3963, migration 130) — an owner-configured allow-list
//     (`agent_invocation_target`), owner-only to write, fail-closed default,
//     no migration for existing rows. `agent_definition_writer` copies it.
//
// Ordering of the three rules, which is the whole control:
//
//  1. actor is a human member  -> unchanged. Existing canManageAgent / role
//     checks decide. The gate never touches humans.
//  2. actor is an agent NOT on the allow-list -> 403, before anything is
//     written. This is the finding, closed.
//  3. actor is an agent ON the allow-list -> permitted, but only for the
//     operations in agentDefinitionScope and only across the field subset in
//     agentDefinitionUpdateFields / agentDefinitionCreateFields. env and
//     mcp_config stay owner-only for every agent, permission_mode /
//     visibility / invocation_targets are never written by an agent (the
//     server sets them on create; update rejects them), and the allow-list
//     itself is human-owner-only (agent_definition_writers_api.go) so no
//     listed seat can extend the list or add itself.
//  4. an agent principal writing ITS OWN record is denied at every scope,
//     allow-listed included. A seat that can rewrite thirty definitions and
//     its own can make its own compromise durable with no owner acting.
//
// The governing access model is recorded in Jartan's internal decision record,
// not in this repository. Section references (§N) in this file are to it.
//
// An EMPTY allow-list is the shipped default and fails closed: no agent
// principal may mutate any agent definition.
//
// Deliberately scoped to `resolveActor`'s "agent" verdict rather than
// `isMachineCredentialActor` (which also covers mcn_ cloud-node PATs). The
// ruled model constrains agent principals; a cloud node registering
// runtimes and provisioning agents on its owner's behalf is a different
// credential class, and denying it here would risk breaking cloud-runtime
// provisioning without closing anything the finding names. If cloud nodes are
// ever used to run agent code in this workspace, widen this predicate.

// agentDefinitionActivityWritten is the activity_log action recorded for every
// successful agent-definition mutation performed BY an allow-listed agent. It
// is evidence, not a control: an audit trail alone does not close the gap this
// gate exists for (write, act, write back and the trail shows two edits with a
// zero diff). It exists so abuse of the one live
// privileged seat is visible after the fact.
const agentDefinitionActivityWritten = "agent_definition_written"

// agentDefinitionDenyMessage is the single 403 body for a denied agent
// principal. Deliberately uniform across every gated endpoint so a caller
// cannot map the surface by comparing error strings.
const agentDefinitionDenyMessage = "agents may not modify agent definitions; ask the workspace owner, or add this agent to the workspace's agent-definition writer allow-list"

// agentDefinitionActor is the resolved caller of a gated request. actorType is
// "member" or "agent"; agentID is set only for the agent case.
type agentDefinitionActor struct {
	actorType string
	agentID   string
}

func (a agentDefinitionActor) isAgent() bool { return a.actorType == "agent" }

// agentDefinitionScope names what a gated route DOES, so the gate can hold the
// amendment's OPERATION scope (§2) and not only its field scope. An
// allow-listed seat may hire, tune, bind skills and retire — that is the whole
// People Ops workflow — and nothing else on the agent-record surface.
//
// The zero value is `Closed`, which is the point: `canManageAgent` takes this
// as an argument, so a mutation route added upstream cannot reach an agent
// principal without someone choosing a scope for it at the call site. On a
// rebase that is a compile error rather than a silently widened gate.
type agentDefinitionScope int

const (
	// agentDefinitionScopeClosed: no agent principal, allow-listed or not.
	// Used for the routes the amendment does not name as permitted —
	// cancel-tasks, runtime-skill overrides, agent labels, Lark bindings.
	agentDefinitionScopeClosed agentDefinitionScope = iota
	// agentDefinitionScopeUpdate: PUT /api/agents/{id}, field-scoped by
	// agentDefinitionUpdateFields.
	agentDefinitionScopeUpdate
	// agentDefinitionScopeSkills: the four workspace-skill writes. Skills are
	// instruction text and People Ops binds them on every hire.
	agentDefinitionScopeSkills
	// agentDefinitionScopeLifecycle: archive / restore — retiring a seat.
	agentDefinitionScopeLifecycle
	// agentDefinitionScopeCreate: POST /api/agents — hiring. Field-scoped by
	// agentDefinitionCreateFields, with the trust axis server-set (§4).
	agentDefinitionScopeCreate
)

// permitsAgent answers whether an allow-listed agent principal may exercise
// this scope at all.
func (s agentDefinitionScope) permitsAgent() bool { return s != agentDefinitionScopeClosed }

// agentDefinitionDenyScopeMessage is the 403 body for a route outside the
// permitted operation set.
const agentDefinitionDenyScopeMessage = "this agent-record operation is reserved to the workspace owner; agents may only create, tune, bind skills to and archive/restore an agent"

// agentDefinitionDenySelfMessage is the 403 body for an agent writing its own
// record (§2). Distinct from the surface-wide message because the caller
// learns nothing about the surface from it — only about itself.
const agentDefinitionDenySelfMessage = "an agent may not modify its own definition; ask the workspace owner"

// authorizeAgentDefinitionWrite is the gate. Call it in every handler that
// mutates an agent record, BEFORE the write and before (or alongside) the
// existing canManageAgent check.
//
// targetAgentID is the record being written, empty on create. scope is what
// the route does.
//
// Returns the resolved actor and ok=false when the caller is denied; the 403
// body is written here, so callers just return. Human callers always pass —
// their authorization is still whatever the handler already enforced.
//
// A database failure on the allow-list lookup denies the write (fail-closed).
// The alternative — permitting a definition write because the allow-list could
// not be read — is exactly the state this gate exists to prevent, and it would
// be invisible.
func (h *Handler) authorizeAgentDefinitionWrite(w http.ResponseWriter, r *http.Request, workspaceID, targetAgentID string, scope agentDefinitionScope) (agentDefinitionActor, bool) {
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	actor := agentDefinitionActor{actorType: actorType, agentID: actorID}
	if !actor.isAgent() {
		return actor, true
	}

	// Self-write, denied ahead of the allow-list because being listed does not
	// buy it (§2). An allow-listed seat that can also rewrite itself can make
	// its own compromise durable without the owner ever acting.
	if targetAgentID != "" && actorID == targetAgentID {
		slog.Warn("agent-definition write denied: agent attempted to write its own record",
			append(logger.RequestAttrs(r), "actor_agent_id", actorID, "workspace_id", workspaceID)...)
		writeError(w, http.StatusForbidden, agentDefinitionDenySelfMessage)
		return actor, false
	}

	if !scope.permitsAgent() {
		slog.Warn("agent-definition write denied: route is outside the permitted operation scope",
			append(logger.RequestAttrs(r), "actor_agent_id", actorID, "workspace_id", workspaceID)...)
		writeError(w, http.StatusForbidden, agentDefinitionDenyScopeMessage)
		return actor, false
	}

	allowed, err := h.isAgentDefinitionWriter(r.Context(), workspaceID, actorID)
	if err != nil {
		slog.Error("agent-definition allow-list lookup failed; denying the write",
			append(logger.RequestAttrs(r), "error", err, "actor_agent_id", actorID)...)
		writeError(w, http.StatusInternalServerError, "could not evaluate the agent-definition writer allow-list; write refused")
		return actor, false
	}
	if !allowed {
		slog.Warn("agent-definition write denied: acting agent is not on the workspace allow-list",
			append(logger.RequestAttrs(r), "actor_agent_id", actorID, "workspace_id", workspaceID)...)
		writeError(w, http.StatusForbidden, agentDefinitionDenyMessage)
		return actor, false
	}
	return actor, true
}

// agentDefinitionUpdateFields is the field scope of PUT /api/agents/{id} for an
// allow-listed agent principal — §2 of the amendment, exactly. Anything else in
// the request body is refused, including a field upstream adds later: this is
// an allow-list, so a new writable field is denied to agents until someone
// decides it is behaviour config rather than a trust axis.
//
// Removed from the pre-amendment list, each with live evidence:
// mcp_config (owner-only alongside env — a tool grant and an egress path, §3),
// custom_args / service_tier / runtime_config (empty on 30/30), runtime_id
// (create only; rebinding a runtime is Platform's call), max_concurrent_tasks
// (no People Ops workflow writes it), status (runtime state, not definition),
// avatar_url (a create-time default, not our write).
var agentDefinitionUpdateFields = map[string]struct{}{
	"name":           {},
	"description":    {},
	"instructions":   {},
	"model":          {},
	"thinking_level": {},
}

// agentDefinitionCreateFields is the same scope for POST /api/agents. It adds
// runtime_id — a required create argument, so permitted there and denied on
// update — plus skill_ids, since binding skills is part of the same hire, and
// template, which is analytics attribution rather than a definition field.
var agentDefinitionCreateFields = map[string]struct{}{
	"name":           {},
	"description":    {},
	"instructions":   {},
	"model":          {},
	"thinking_level": {},
	"runtime_id":     {},
	"skill_ids":      {},
	"template":       {},
}

// agentDefinitionServerSetFields are the trust-axis inputs an agent principal
// may submit and never write: on create the server sets them and IGNORES what
// the caller sent (§4), so they are neither honoured nor refused.
//
// Ignore rather than reject is load-bearing. `multica agent create
// --visibility` carries a default value, and a CLI release that serialises a
// defaulted field would meet a fail-loud rejection on every create — the
// silent CLI/server skew this fork exists to avoid (AC8). Confirmed at the
// pinned release: the CLI sends these keys only when the flag was explicitly
// set. The rule does not depend on that holding.
var agentDefinitionServerSetFields = map[string]struct{}{
	"permission_mode":    {},
	"visibility":         {},
	"invocation_targets": {},
}

// denyRestrictedAgentDefinitionFields enforces the field scope. It reports
// ok=false and writes the 403 when the body carries a field this principal may
// not write. permitted is the scope's allow-list; ignoreServerSet is true on
// create, where the trust axis is dropped rather than refused.
//
// Only rawFields is consulted, so the check is presence-based: a client that
// omits a field is unaffected, and one that sends it — with any value, equal to
// the current value or not — is refused. On update that is the amendment's
// literal rule for the permission axis ("403, fail loud"), and the same rule
// reads consistently for every other denied field.
func denyRestrictedAgentDefinitionFields(w http.ResponseWriter, rawFields map[string]json.RawMessage, permitted map[string]struct{}, ignoreServerSet bool) bool {
	for field := range rawFields {
		if _, ok := permitted[field]; ok {
			continue
		}
		if _, serverSet := agentDefinitionServerSetFields[field]; serverSet {
			if ignoreServerSet {
				continue
			}
			writeError(w, http.StatusForbidden, "only the workspace owner can change access (permission_mode / visibility / invocation_targets)")
			return false
		}
		writeError(w, http.StatusForbidden, "agents may not write "+field+" on an agent definition; that field is reserved to the workspace owner")
		return false
	}
	return true
}

// agentCreatedSeatPermission is what the server mints for a seat created by an
// allow-listed agent principal: public_to with the workspace as the sole
// invocation target (§4).
//
// This is the only shape the roster has ever had — 30 of 30 seats — and it is
// what makes a new hire assignable. The pre-amendment rule refused caller
// permission input instead, which landed every hire at the create default of
// `private`: unassignable, needing the owner to flip a field per hire.
//
// Built here rather than parsed from the request on purpose. A value-parsing
// branch on the trust axis is where a validation gap widens invocation; this
// accepts no agent input at all.
func agentCreatedSeatPermission(workspaceID pgtype.UUID) resolvedPermission {
	return resolvedPermission{
		mode:    permissionModePublicTo,
		targets: []targetSpec{{targetType: invocationTargetWorkspace, targetID: workspaceID}},
	}
}

// agentDefinitionActorOf resolves the calling principal without authorizing
// anything. Use it at a point already past the gate — to record who wrote, or
// to answer "is this caller an agent?" for a field-scope decision. The mat_
// path is a header read, so this costs nothing on the hot path.
func (h *Handler) agentDefinitionActorOf(r *http.Request, workspaceID string) agentDefinitionActor {
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	return agentDefinitionActor{actorType: actorType, agentID: actorID}
}

// isAgentDefinitionWriter answers the allow-list membership question. An
// unparseable workspace or agent id is not on any list — the ids come from a
// server-validated token and a resolved agent record, so a parse failure means
// something is wrong upstream, and the safe answer is "denied".
func (h *Handler) isAgentDefinitionWriter(ctx context.Context, workspaceID, agentID string) (bool, error) {
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return false, err
	}
	agentUUID, err := util.ParseUUID(agentID)
	if err != nil {
		return false, err
	}
	return h.Queries.IsAgentDefinitionWriter(ctx, db.IsAgentDefinitionWriterParams{
		WorkspaceID: wsUUID,
		AgentID:     agentUUID,
	})
}

// auditAgentDefinitionWrite records that an allow-listed agent mutated an
// agent definition. Called after a successful write; a no-op for human callers,
// whose writes are already attributable to a human identity.
//
// Deliberately NOT fail-closed, unlike the env reveal audit (agent_env.go):
// there the audit row is the only record that a secret was disclosed, so a lost
// row loses the evidence irrecoverably. Here the write itself is already
// committed by the time we get here, and failing the response would tell the
// caller the write did not happen when it did. A failed audit row is logged at
// ERROR instead.
func (h *Handler) auditAgentDefinitionWrite(r *http.Request, actor agentDefinitionActor, workspaceID pgtype.UUID, target db.Agent, action string) {
	if !actor.isAgent() {
		return
	}
	details, _ := json.Marshal(map[string]any{
		"target_agent_id":   uuidToString(target.ID),
		"target_agent_name": target.Name,
		"operation":         action,
		"actor_agent_id":    actor.agentID,
	})
	if _, err := h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: workspaceID,
		IssueID:     pgtype.UUID{}, // a definition write is not tied to an issue
		ActorType:   pgtype.Text{String: "agent", Valid: true},
		ActorID:     parseUUID(actor.agentID),
		Action:      agentDefinitionActivityWritten,
		Details:     details,
	}); err != nil {
		slog.Error("agent_definition_written audit write failed",
			append(logger.RequestAttrs(r), "error", err, "actor_agent_id", actor.agentID,
				"target_agent_id", uuidToString(target.ID), "operation", action)...)
	}
}
