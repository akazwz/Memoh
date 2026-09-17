-- 0153_grok_runtime: reverse Grok identity and immutable checkpoint storage.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM public.agent_session_states WHERE storage_revision <> '00000000-0000-0000-0000-000000000000')
    OR EXISTS (SELECT 1 FROM public.agent_session_state_lines WHERE storage_revision <> '00000000-0000-0000-0000-000000000000')
    OR EXISTS (SELECT 1 FROM public.agent_session_fork_states)
  THEN RAISE EXCEPTION 'remove immutable native checkpoints and fork seeds before rollback'; END IF;
END $$;
DROP TABLE IF EXISTS public.agent_session_fork_states;

-- Restore the previous runtime vocabulary and shared checkpoint layout.
-- Rollback requires Grok Agents and their sessions to be removed first.

ALTER TABLE public.bots DROP CONSTRAINT IF EXISTS bots_chat_runtime_check;

ALTER TABLE public.bots ADD CONSTRAINT bots_chat_runtime_check CHECK (chat_runtime IN ('model', 'acp_agent', 'codex', 'claude-code'));

ALTER TABLE public.bot_sessions DROP CONSTRAINT IF EXISTS bot_sessions_runtime_type_check;

ALTER TABLE public.bot_sessions ADD CONSTRAINT bot_sessions_runtime_type_check CHECK (runtime_type IN ('model', 'acp_agent', 'codex', 'claude-code'));

ALTER TABLE public.bot_history_messages DROP CONSTRAINT IF EXISTS bot_history_messages_runtime_type_check;

ALTER TABLE public.bot_history_messages ADD CONSTRAINT bot_history_messages_runtime_type_check CHECK (runtime_type IN ('model', 'acp_agent', 'codex', 'claude-code'));

ALTER TABLE public.schedule DROP CONSTRAINT IF EXISTS schedule_runtime_type_check;

ALTER TABLE public.schedule ADD CONSTRAINT schedule_runtime_type_check CHECK (runtime_type IS NULL OR runtime_type IN ('model', 'acp_agent', 'codex', 'claude-code'));

ALTER TABLE public.agent_authorizations DROP CONSTRAINT IF EXISTS agent_authorizations_runtime_check;

ALTER TABLE public.agent_authorizations ADD CONSTRAINT agent_authorizations_runtime_check CHECK (runtime IN ('codex', 'claude-code'));

ALTER TABLE public.schedule DROP CONSTRAINT IF EXISTS schedule_acp_fields_check;

ALTER TABLE public.schedule ADD CONSTRAINT schedule_acp_fields_check CHECK (
    run_target <> 'new_session'
    OR (runtime_type = 'acp_agent' AND acp_agent_id IS NOT NULL AND model_id IS NULL)
    OR (runtime_type IN ('codex', 'claude-code') AND bot_agent_id IS NOT NULL AND acp_agent_id IS NULL AND model_id IS NULL)
    OR (COALESCE(runtime_type, 'model') = 'model' AND bot_agent_id IS NULL AND acp_agent_id IS NULL AND acp_model_id IS NULL)
  );


ALTER TABLE public.agent_session_state_lines DROP CONSTRAINT IF EXISTS agent_session_state_lines_pkey;

ALTER TABLE public.agent_session_state_lines DROP COLUMN IF EXISTS storage_revision;

ALTER TABLE public.agent_session_state_lines ADD PRIMARY KEY (team_id, session_id, file_path, line_number);

ALTER TABLE public.agent_session_states DROP COLUMN IF EXISTS storage_revision;
