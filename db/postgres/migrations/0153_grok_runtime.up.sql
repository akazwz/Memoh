-- 0153_grok_runtime
-- Add Grok Build and isolate mutable native checkpoints by immutable revision.

ALTER TABLE public.bots DROP CONSTRAINT IF EXISTS bots_chat_runtime_check;

ALTER TABLE public.bots ADD CONSTRAINT bots_chat_runtime_check CHECK (chat_runtime IN ('model', 'acp_agent', 'codex', 'claude-code', 'grok'));

ALTER TABLE public.bot_sessions DROP CONSTRAINT IF EXISTS bot_sessions_runtime_type_check;

ALTER TABLE public.bot_sessions ADD CONSTRAINT bot_sessions_runtime_type_check CHECK (runtime_type IN ('model', 'acp_agent', 'codex', 'claude-code', 'grok'));

ALTER TABLE public.bot_history_messages DROP CONSTRAINT IF EXISTS bot_history_messages_runtime_type_check;

ALTER TABLE public.bot_history_messages ADD CONSTRAINT bot_history_messages_runtime_type_check CHECK (runtime_type IN ('model', 'acp_agent', 'codex', 'claude-code', 'grok'));

ALTER TABLE public.schedule DROP CONSTRAINT IF EXISTS schedule_runtime_type_check;

ALTER TABLE public.schedule ADD CONSTRAINT schedule_runtime_type_check CHECK (runtime_type IS NULL OR runtime_type IN ('model', 'acp_agent', 'codex', 'claude-code', 'grok'));

ALTER TABLE public.agent_authorizations DROP CONSTRAINT IF EXISTS agent_authorizations_runtime_check;

ALTER TABLE public.agent_authorizations ADD CONSTRAINT agent_authorizations_runtime_check CHECK (runtime IN ('codex', 'claude-code', 'grok'));

ALTER TABLE public.schedule DROP CONSTRAINT IF EXISTS schedule_acp_fields_check;

ALTER TABLE public.schedule ADD CONSTRAINT schedule_acp_fields_check CHECK (
    run_target <> 'new_session'
    OR (runtime_type = 'acp_agent' AND acp_agent_id IS NOT NULL AND model_id IS NULL)
    OR (runtime_type IN ('codex', 'claude-code', 'grok') AND bot_agent_id IS NOT NULL AND acp_agent_id IS NULL AND model_id IS NULL)
    OR (COALESCE(runtime_type, 'model') = 'model' AND bot_agent_id IS NULL AND acp_agent_id IS NULL AND acp_model_id IS NULL)
  );

ALTER TABLE public.agent_session_states ADD COLUMN IF NOT EXISTS storage_revision UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

ALTER TABLE public.agent_session_state_lines ADD COLUMN IF NOT EXISTS storage_revision UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';

ALTER TABLE public.agent_session_state_lines DROP CONSTRAINT IF EXISTS agent_session_state_lines_pkey;

ALTER TABLE public.agent_session_state_lines ADD PRIMARY KEY (team_id, session_id, storage_revision, file_path, line_number);

-- A fork has no Run of its own yet. Its cold-start seed is copied atomically
-- with visible history, independently of later source publications/deletion.
CREATE TABLE IF NOT EXISTS public.agent_session_fork_states (
    LIKE public.agent_session_states INCLUDING DEFAULTS INCLUDING CONSTRAINTS,
    PRIMARY KEY (team_id, session_id),
    FOREIGN KEY (team_id, session_id)
        REFERENCES public.bot_sessions(team_id, id) ON DELETE CASCADE
);
ALTER TABLE public.agent_session_fork_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.agent_session_fork_states FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS agent_session_fork_states_team ON public.agent_session_fork_states;
CREATE POLICY agent_session_fork_states_team ON public.agent_session_fork_states
    USING (team_id = public.memoh_current_team_id())
    WITH CHECK (team_id = public.memoh_current_team_id());
