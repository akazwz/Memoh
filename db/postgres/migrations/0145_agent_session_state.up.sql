-- 0145_agent_session_state
-- Generalize the ACP-named checkpoint storage to External Agent names so
-- ACP, Codex, and Claude Code share one store.
--
-- Two paths per table, both idempotent:
--   * Upgrade (only acp_* exists): pure rename — data, constraints, and
--     policies are preserved.
--   * Fresh chain (both exist): 0001 already created the final agent_*
--     schema and the frozen 0138 recreated the legacy acp_* tables empty
--     afterwards; drop the unused empty duplicates. A non-empty duplicate is
--     an impossible state and fails loudly instead of losing data.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'acp_session_states'
    ) THEN
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'public' AND table_name = 'agent_session_states'
        ) THEN
            -- The probe scans a table under FORCE row level security; lift it
            -- first (the table is dropped below, and an abort rolls this back).
            ALTER TABLE public.acp_session_states NO FORCE ROW LEVEL SECURITY;
            ALTER TABLE public.acp_session_states DISABLE ROW LEVEL SECURITY;
            IF EXISTS (SELECT 1 FROM public.acp_session_states) THEN
                RAISE EXCEPTION 'both acp_session_states and agent_session_states exist and the legacy table has data';
            END IF;
            DROP TABLE public.acp_session_states;
        ELSE
            ALTER TABLE public.acp_session_states RENAME TO agent_session_states;
            ALTER TABLE public.agent_session_states RENAME COLUMN acp_session_id TO agent_session_id;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_pkey TO agent_session_states_pkey;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_session_id_fkey TO agent_session_states_session_id_fkey;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_run_fkey TO agent_session_states_run_fkey;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_agent_id_check TO agent_session_states_agent_id_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_acp_session_id_check TO agent_session_states_agent_session_id_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_cwd_check TO agent_session_states_cwd_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_transcript_path_check TO agent_session_states_transcript_path_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_runtime_fencing_token_check TO agent_session_states_runtime_fencing_token_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_file_count_check TO agent_session_states_file_count_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_record_count_check TO agent_session_states_record_count_check;
            ALTER TABLE public.agent_session_states RENAME CONSTRAINT acp_session_states_file_shapes_check TO agent_session_states_file_shapes_check;
            ALTER POLICY acp_session_states_team_select ON public.agent_session_states RENAME TO agent_session_states_team_select;
            ALTER POLICY acp_session_states_team_insert ON public.agent_session_states RENAME TO agent_session_states_team_insert;
            ALTER POLICY acp_session_states_team_update ON public.agent_session_states RENAME TO agent_session_states_team_update;
            ALTER POLICY acp_session_states_team_delete ON public.agent_session_states RENAME TO agent_session_states_team_delete;
        END IF;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'acp_session_state_lines'
    ) THEN
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'public' AND table_name = 'agent_session_state_lines'
        ) THEN
            -- The probe scans a table under FORCE row level security; lift it
            -- first (the table is dropped below, and an abort rolls this back).
            ALTER TABLE public.acp_session_state_lines NO FORCE ROW LEVEL SECURITY;
            ALTER TABLE public.acp_session_state_lines DISABLE ROW LEVEL SECURITY;
            IF EXISTS (SELECT 1 FROM public.acp_session_state_lines) THEN
                RAISE EXCEPTION 'both acp_session_state_lines and agent_session_state_lines exist and the legacy table has data';
            END IF;
            DROP TABLE public.acp_session_state_lines;
        ELSE
            ALTER TABLE public.acp_session_state_lines RENAME TO agent_session_state_lines;
            ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT acp_session_state_lines_pkey TO agent_session_state_lines_pkey;
            ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT acp_session_state_lines_session_fkey TO agent_session_state_lines_session_fkey;
            ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT acp_session_state_lines_file_path_check TO agent_session_state_lines_file_path_check;
            ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT acp_session_state_lines_content_size_check TO agent_session_state_lines_content_size_check;
            ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT acp_session_state_lines_line_number_check TO agent_session_state_lines_line_number_check;
            ALTER POLICY acp_session_state_lines_team_select ON public.agent_session_state_lines RENAME TO agent_session_state_lines_team_select;
            ALTER POLICY acp_session_state_lines_team_insert ON public.agent_session_state_lines RENAME TO agent_session_state_lines_team_insert;
            ALTER POLICY acp_session_state_lines_team_update ON public.agent_session_state_lines RENAME TO agent_session_state_lines_team_update;
            ALTER POLICY acp_session_state_lines_team_delete ON public.agent_session_state_lines RENAME TO agent_session_state_lines_team_delete;
        END IF;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'acp_session_publications'
    ) THEN
        IF EXISTS (
            SELECT 1 FROM information_schema.tables
            WHERE table_schema = 'public' AND table_name = 'agent_session_publications'
        ) THEN
            -- The probe scans a table under FORCE row level security; lift it
            -- first (the table is dropped below, and an abort rolls this back).
            ALTER TABLE public.acp_session_publications NO FORCE ROW LEVEL SECURITY;
            ALTER TABLE public.acp_session_publications DISABLE ROW LEVEL SECURITY;
            IF EXISTS (SELECT 1 FROM public.acp_session_publications) THEN
                RAISE EXCEPTION 'both acp_session_publications and agent_session_publications exist and the legacy table has data';
            END IF;
            DROP TABLE public.acp_session_publications;
        ELSE
            ALTER TABLE public.acp_session_publications RENAME TO agent_session_publications;
            ALTER TABLE public.agent_session_publications RENAME CONSTRAINT acp_session_publications_pkey TO agent_session_publications_pkey;
            ALTER TABLE public.agent_session_publications RENAME CONSTRAINT acp_session_publications_session_fkey TO agent_session_publications_session_fkey;
            ALTER TABLE public.agent_session_publications RENAME CONSTRAINT acp_session_publications_run_fkey TO agent_session_publications_run_fkey;
            ALTER POLICY acp_session_publications_team_select ON public.agent_session_publications RENAME TO agent_session_publications_team_select;
            ALTER POLICY acp_session_publications_team_insert ON public.agent_session_publications RENAME TO agent_session_publications_team_insert;
            ALTER POLICY acp_session_publications_team_update ON public.agent_session_publications RENAME TO agent_session_publications_team_update;
            ALTER POLICY acp_session_publications_team_delete ON public.agent_session_publications RENAME TO agent_session_publications_team_delete;
        END IF;
    END IF;
END $$;
