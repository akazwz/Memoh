-- 0146_agent_turn_markers
-- Restore the ACP-era spellings of the reconciliation marker keys.

ALTER TABLE public.bot_history_messages NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_history_messages DISABLE ROW LEVEL SECURITY;

UPDATE public.bot_history_messages
SET metadata = (metadata - 'agent_turn_outcome')
    || jsonb_build_object('acp_turn_outcome', metadata->'agent_turn_outcome')
WHERE metadata ? 'agent_turn_outcome';

UPDATE public.bot_history_messages
SET metadata = (metadata - 'agent_decision_projection')
    || jsonb_build_object('acp_decision_projection', metadata->'agent_decision_projection')
WHERE metadata ? 'agent_decision_projection';

UPDATE public.bot_history_messages
SET metadata = (metadata - 'agent_decision_tool_call_id')
    || jsonb_build_object('acp_decision_tool_call_id', metadata->'agent_decision_tool_call_id')
WHERE metadata ? 'agent_decision_tool_call_id';

ALTER TABLE public.bot_history_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_history_messages FORCE ROW LEVEL SECURITY;
