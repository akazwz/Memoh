-- 0093_tool_approval_generic_names
-- ACP permission requests that don't map onto a client-capability tool are
-- approved under synthetic acp_permission[:kind] names; widen the tool_name
-- check to admit them.

ALTER TABLE tool_approval_requests
  DROP CONSTRAINT tool_approval_tool_name_check;

ALTER TABLE tool_approval_requests
  ADD CONSTRAINT tool_approval_tool_name_check
  CHECK (tool_name IN ('write', 'edit', 'exec') OR tool_name LIKE 'acp_permission%');
