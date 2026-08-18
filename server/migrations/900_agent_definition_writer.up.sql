-- Agent-definition write gate: the owner-configured allow-list of agents that
-- may mutate agent definitions (Jartan fork, SEC-2026-0069).
--
-- Upstream lets any principal that clears `canManageAgent` (the agent's owner
-- OR a workspace owner/admin) rewrite any agent's behaviour-defining fields.
-- A running agent authenticates with a mat_ task token whose X-User-ID is its
-- OWNING human, so on a single-owner workspace every agent clears that check
-- for every other agent: one turn of influence over any agent converts into
-- permanent control of a seat. The gate applies the actor-type predicate the
-- env endpoints already use (`resolveActor` -> "agent") to the whole
-- agent-record mutation surface, and this table is the exception list.
--
-- Shape copied from migration 130 (agent_invocation_target, the `specific_agents`
-- pattern): an owner-configured allow-list, no migration for existing rows, and
-- a fail-closed default — an EMPTY table means no agent may mutate any agent
-- definition, which is also the shipped default. The list is writable only by a
-- human workspace owner, so no listed agent can extend it or add itself.
--
-- NO foreign keys by design (Multica migration rule, same as migration 130):
--   * workspace_id / agent_id / created_by relationships are maintained in the
--     application layer. A stale row is inert — the gate resolves the acting
--     agent id from the request's own task token, so a row naming a deleted
--     agent grants nothing.
--
-- Prefix 900 is deliberate and is NOT the next sequential number: it keeps the
-- fork's own migrations in a reserved band above upstream's, so a rebase never
-- collides with an upstream migration that took the same number. Migrations are
-- tracked per-version in schema_migrations (cmd/migrate/main.go), not by a
-- single version cursor, so an out-of-band prefix applies and rolls back
-- normally and does not suppress later upstream migrations.
CREATE TABLE agent_definition_writer (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    agent_id     UUID NOT NULL,
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, agent_id)
);

COMMENT ON TABLE agent_definition_writer IS
    'Allow-list of agents permitted to mutate agent definitions in a workspace (Jartan fork, SEC-2026-0069). One row per (workspace, agent). Empty = fail closed: no agent principal may mutate any agent definition, only human members. Writable only by a human workspace owner; a listed agent gets the behaviour-config subset only (never permission_mode / visibility / invocation_targets / env). The UNIQUE (workspace_id, agent_id) index is also the lookup index — workspace_id leads, so both the per-workspace list and the per-agent membership check use it. No DB foreign keys: relationships are maintained in the application layer (see migration comment).';
