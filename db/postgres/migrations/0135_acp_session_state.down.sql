-- 0135_acp_session_state
-- Remove durable ACP session snapshots.

DROP TABLE IF EXISTS public.acp_session_state_lines;
DROP TABLE IF EXISTS public.acp_session_publications;
DROP TABLE IF EXISTS public.acp_session_states;
ALTER TABLE IF EXISTS public.session_runs
    DROP CONSTRAINT IF EXISTS session_runs_team_session_run_key;
