-- 0093_tool_approval_generic_names (down)

DELETE FROM tool_approval_requests
WHERE tool_name NOT IN ('write', 'edit', 'exec');

ALTER TABLE tool_approval_requests
  DROP CONSTRAINT tool_approval_tool_name_check;

ALTER TABLE tool_approval_requests
  ADD CONSTRAINT tool_approval_tool_name_check
  CHECK (tool_name IN ('write', 'edit', 'exec'));
