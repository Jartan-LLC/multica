-- Record the SENDING AGENT on a chat-initiated run (Jartan fork, SEC-2026-0079).
--
-- `agent_task_queue` carries `initiator_user_id` and nothing else for "who sent
-- the message that started this run" (migration 117). When an agent run sends a
-- chat message, the server resolves the sending principal correctly on the way
-- in (`resolveActor` -> ("agent", id), from a header the auth middleware forces
-- off the task token) and then drops it: the row is stamped with the owning
-- human, `attribution.DirectHumanRun` marks it source `direct_human` — which the
-- attribution package classifies as compliance-grade precise — and at claim the
-- daemon tells the recipient "This task was initiated by <owner>, a member of
-- this workspace".
--
-- So the record does not omit the sender, it asserts a false one, and tells the
-- recipient the same thing in the one prompt block that exists to answer "who am
-- I answering". This column closes that gap: one nullable id, written from the
-- task token, never from anything the caller asserted.
--
-- NULL means "no agent sent this" — every pre-existing row and every run a human
-- starts. No backfill: the sending principal of a historical row is not
-- recoverable, and inventing one would be the same false claim in the other
-- direction.
--
-- Mutually exclusive with initiator_user_id in practice, not by constraint: an
-- agent send writes this column and leaves initiator_user_id NULL (no human sent
-- it), a member send does the reverse. Left unconstrained so an upstream path
-- that legitimately carries both does not fail on insert.
--
-- NO foreign key by design (Multica migration rule, as migrations 900/901):
-- the agent relationship is maintained in the application layer. No index
-- either: the column is read per-row at claim, by task id, and nothing queries
-- the queue BY initiator.
--
-- Prefix 902 is deliberate and is NOT the next sequential number: fork
-- migrations live in a reserved band above upstream's so a rebase never
-- collides with an upstream migration that took the same number.
ALTER TABLE agent_task_queue
    ADD COLUMN initiator_agent_id UUID NULL;

COMMENT ON COLUMN agent_task_queue.initiator_agent_id IS
    'The agent that sent the chat message which enqueued this run, or NULL when no agent did (Jartan fork, SEC-2026-0079). Server-verified: written from the mat_ task token the auth middleware bound to the request, never from caller-supplied input. Read at claim to render the initiator as an agent instead of as the token''s owning human. Not a foreign key — the relationship is maintained in the application layer.';
