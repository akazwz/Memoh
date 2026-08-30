-- 0146_agent_turn_markers
-- The commit-unknown reconciliation markers started as ACP vocabulary but are
-- written by every non-model runtime; rename the persisted metadata keys to
-- match (acp_turn_outcome -> agent_turn_outcome, acp_decision_projection ->
-- agent_decision_projection, acp_decision_tool_call_id ->
-- agent_decision_tool_call_id).

-- The rewrite scans a table under FORCE row level security keyed on
-- memoh.team_id, which a migration connection never sets. Lift the policy for
-- the data step and restore it after, the same way 0125/0128 do.
ALTER TABLE public.bot_history_messages NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bot_history_messages DISABLE ROW LEVEL SECURITY;

UPDATE public.bot_history_messages
SET metadata = (metadata - 'acp_turn_outcome')
    || jsonb_build_object('agent_turn_outcome', metadata->'acp_turn_outcome')
WHERE metadata ? 'acp_turn_outcome';

UPDATE public.bot_history_messages
SET metadata = (metadata - 'acp_decision_projection')
    || jsonb_build_object('agent_decision_projection', metadata->'acp_decision_projection')
WHERE metadata ? 'acp_decision_projection';

UPDATE public.bot_history_messages
SET metadata = (metadata - 'acp_decision_tool_call_id')
    || jsonb_build_object('agent_decision_tool_call_id', metadata->'acp_decision_tool_call_id')
WHERE metadata ? 'acp_decision_tool_call_id';

ALTER TABLE public.bot_history_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bot_history_messages FORCE ROW LEVEL SECURITY;
