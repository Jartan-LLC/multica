-- Agent-definition write allow-list (Jartan fork). Rows name the
-- agents permitted to mutate agent definitions in a workspace. See migration 900
-- and internal/handler/agent_definition_gate.go.

-- name: ListAgentDefinitionWriters :many
SELECT * FROM agent_definition_writer
WHERE workspace_id = $1
ORDER BY created_at ASC, agent_id ASC;

-- name: IsAgentDefinitionWriter :one
-- The gate's hot path: one indexed lookup on the UNIQUE (workspace_id, agent_id)
-- key per mutating request. Scoped to the workspace so a grant in one workspace
-- never carries into another.
SELECT EXISTS (
    SELECT 1 FROM agent_definition_writer
    WHERE workspace_id = @workspace_id AND agent_id = @agent_id
) AS exists;

-- name: CreateAgentDefinitionWriter :exec
-- Idempotent upsert so a re-submitted list is not an error. Callers replace the
-- whole list via DeleteAgentDefinitionWriters + a series of these, inside one
-- transaction, matching the agent_invocation_target write model (migration 130).
INSERT INTO agent_definition_writer (workspace_id, agent_id, created_by)
VALUES ($1, $2, sqlc.narg('created_by'))
ON CONFLICT (workspace_id, agent_id) DO UPDATE SET
    created_by = EXCLUDED.created_by,
    created_at = now();

-- name: DeleteAgentDefinitionWriters :exec
-- Clears the whole allow-list for a workspace, ahead of a wholesale replace.
DELETE FROM agent_definition_writer
WHERE workspace_id = $1;
