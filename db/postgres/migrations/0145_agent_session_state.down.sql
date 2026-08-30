-- 0145_agent_session_state
-- Revert the External Agent renames back to the ACP-scoped names. Each
-- block is guarded on the new table name so the rollback is safe to re-run.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'agent_session_publications'
    ) THEN
        ALTER POLICY agent_session_publications_team_select ON public.agent_session_publications RENAME TO acp_session_publications_team_select;
        ALTER POLICY agent_session_publications_team_insert ON public.agent_session_publications RENAME TO acp_session_publications_team_insert;
        ALTER POLICY agent_session_publications_team_update ON public.agent_session_publications RENAME TO acp_session_publications_team_update;
        ALTER POLICY agent_session_publications_team_delete ON public.agent_session_publications RENAME TO acp_session_publications_team_delete;
        ALTER TABLE public.agent_session_publications RENAME CONSTRAINT agent_session_publications_pkey TO acp_session_publications_pkey;
        ALTER TABLE public.agent_session_publications RENAME CONSTRAINT agent_session_publications_session_fkey TO acp_session_publications_session_fkey;
        ALTER TABLE public.agent_session_publications RENAME CONSTRAINT agent_session_publications_run_fkey TO acp_session_publications_run_fkey;
        ALTER TABLE public.agent_session_publications RENAME TO acp_session_publications;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'agent_session_state_lines'
    ) THEN
        ALTER POLICY agent_session_state_lines_team_select ON public.agent_session_state_lines RENAME TO acp_session_state_lines_team_select;
        ALTER POLICY agent_session_state_lines_team_insert ON public.agent_session_state_lines RENAME TO acp_session_state_lines_team_insert;
        ALTER POLICY agent_session_state_lines_team_update ON public.agent_session_state_lines RENAME TO acp_session_state_lines_team_update;
        ALTER POLICY agent_session_state_lines_team_delete ON public.agent_session_state_lines RENAME TO acp_session_state_lines_team_delete;
        ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT agent_session_state_lines_pkey TO acp_session_state_lines_pkey;
        ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT agent_session_state_lines_session_fkey TO acp_session_state_lines_session_fkey;
        ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT agent_session_state_lines_file_path_check TO acp_session_state_lines_file_path_check;
        ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT agent_session_state_lines_content_size_check TO acp_session_state_lines_content_size_check;
        ALTER TABLE public.agent_session_state_lines RENAME CONSTRAINT agent_session_state_lines_line_number_check TO acp_session_state_lines_line_number_check;
        ALTER TABLE public.agent_session_state_lines RENAME TO acp_session_state_lines;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'agent_session_states'
    ) THEN
        ALTER POLICY agent_session_states_team_select ON public.agent_session_states RENAME TO acp_session_states_team_select;
        ALTER POLICY agent_session_states_team_insert ON public.agent_session_states RENAME TO acp_session_states_team_insert;
        ALTER POLICY agent_session_states_team_update ON public.agent_session_states RENAME TO acp_session_states_team_update;
        ALTER POLICY agent_session_states_team_delete ON public.agent_session_states RENAME TO acp_session_states_team_delete;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_pkey TO acp_session_states_pkey;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_session_id_fkey TO acp_session_states_session_id_fkey;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_run_fkey TO acp_session_states_run_fkey;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_agent_id_check TO acp_session_states_agent_id_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_agent_session_id_check TO acp_session_states_acp_session_id_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_cwd_check TO acp_session_states_cwd_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_transcript_path_check TO acp_session_states_transcript_path_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_runtime_fencing_token_check TO acp_session_states_runtime_fencing_token_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_file_count_check TO acp_session_states_file_count_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_record_count_check TO acp_session_states_record_count_check;
        ALTER TABLE public.agent_session_states RENAME CONSTRAINT agent_session_states_file_shapes_check TO acp_session_states_file_shapes_check;
        ALTER TABLE public.agent_session_states RENAME COLUMN agent_session_id TO acp_session_id;
        ALTER TABLE public.agent_session_states RENAME TO acp_session_states;
    END IF;
END $$;
