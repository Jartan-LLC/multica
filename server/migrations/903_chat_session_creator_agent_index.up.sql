-- The chat actor gate's list filter (Jartan fork) asks a
-- workspace's sessions which ones a given agent created. Partial because
-- agent-created sessions are a small minority of the table — every session a
-- human opened carries NULL here. Built concurrently so the established
-- chat_session write path stays available throughout rollout; a single
-- statement is required because CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_creator_agent
    ON chat_session (creator_agent_id, workspace_id)
    WHERE creator_agent_id IS NOT NULL;
