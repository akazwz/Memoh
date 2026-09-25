-- 0156_remove_native_session_checkpoints
-- Remove obsolete native conversation snapshots; keep round-publication fencing.
-- This permanently deletes snapshots; rollback restores only empty tables.

DROP TABLE IF EXISTS public.agent_session_state_lines;
DROP TABLE IF EXISTS public.agent_session_states;
ALTER TABLE public.agent_session_publications DROP COLUMN IF EXISTS checkpoint_reset;

-- Pre-0145 names are absent on upgraded installations; remove any legacy remnants.
DROP TABLE IF EXISTS public.acp_session_state_lines;
DROP TABLE IF EXISTS public.acp_session_states;
DROP TABLE IF EXISTS public.acp_session_publications;
