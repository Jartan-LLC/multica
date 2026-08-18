-- Chat-session access by principal: record WHICH agent created a session
-- (Jartan fork, SEC-2026-0078).
--
-- Upstream authorises chat on the session's creator alone —
-- `loadChatSessionForUser` admits when `chat_session.creator_id = X-User-ID`.
-- An agent run authenticates with a `mat_` task token whose `X-User-ID` is
-- stamped to its OWNING HUMAN, so in a workspace where one human owns every
-- agent, every seat and the owner resolve to the same id: the creator
-- predicate separates nobody from anybody, and any run can read every chat
-- session in the workspace, transcripts included.
--
-- The fix scopes chat authorisation by principal instead. For a task-token
-- request a session is reachable when the calling agent is either the
-- session's PINNED agent (`agent_id` — its own conversation) or the agent
-- whose run CREATED it. `agent_id` already exists; this column carries the
-- second half. Member requests are unchanged.
--
-- NULL means "created by a human", which is every pre-existing row and every
-- row the web UI writes. No backfill: NULL is the correct value for all of
-- them, and it is also the fail-closed value — a NULL row is reachable by an
-- agent principal only when that agent is the session's pinned agent.
--
-- NO foreign key by design (Multica migration rule, as migration 900): the
-- agent relationship is maintained in the application layer. A row naming a
-- deleted agent grants nothing — the gate compares against the agent id the
-- request's own task token resolved to, and a deleted agent has no runs.
--
-- Its index is migration 903, built concurrently in its own single-statement
-- file per the repository's migration rule.
--
-- Prefix 901 is deliberate and is NOT the next sequential number: fork
-- migrations live in a reserved band above upstream's so a rebase never
-- collides with an upstream migration that took the same number.
ALTER TABLE chat_session
    ADD COLUMN creator_agent_id UUID NULL;

COMMENT ON COLUMN chat_session.creator_agent_id IS
    'The agent whose run created this session, or NULL when a human created it (Jartan fork, SEC-2026-0078). Read by the chat actor gate: a task-token request reaches a session only when the calling agent is the session''s agent_id or its creator_agent_id. Not a foreign key — the relationship is maintained in the application layer.';
