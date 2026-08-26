-- testdata/zcode/fixture.sql
--
-- SYNTHETIC fixture for the zcode (Z.AI) adapter, built against the schema
-- captured in testdata/zcode/schema.sql (six adapter-consumed tables only:
-- schema_migration, session, message, part, todo, model_usage).
--
-- Every id/path/prompt/tool-output string below is invented for this
-- fixture -- NONE of it is copied from a real ~/.zcode/cli/db/db.sqlite
-- capture. The *shapes* (JSON field names, tool names, part types, the
-- model_usage netting arithmetic) are grounded against a live re-parse of
-- two real installs on 2026-08-25 (zcode-app-cli 3.8.1-15 / zcode-runtime
-- 0.16.3, one native WSL store + one foreign-mount Windows store) -- see
-- docs/zcode-adapter.md and testdata/zcode/README.md for the live-grounding
-- notes. Timestamps are epoch milliseconds (matching zcode's own
-- convention), arbitrarily anchored at 2026-08-24T00:26:40Z (1756000000000)
-- with plausible deltas; they carry no real-world significance.
--
-- Covers, deliberately, every part/tool shape this adapter's loaders
-- either capture or intentionally drop:
--   - a user prompt (loadUserPromptEvents)
--   - tool calls for Read / Bash (one failing exit=1) / Edit / Write /
--     WebSearch / Agent (subagent spawn) (loadToolEvents / mapTool)
--   - a reasoning part threaded onto its successor tool call
--     (loadReasoningIndex)
--   - step-start and timeline parts, which NO loader queries -- silently
--     dropped by design (see docs/zcode-adapter.md "Known gaps")
--   - a step-finish part, surfaced as an observability ToolEvent only,
--     never as a TokenEvent (loadStepFinishEvents)
--   - an assistant text part (loadAssistantTextEvents) and the
--     assistant.stop completion marker (loadCompletionEvents)
--   - a genuinely separate child session linked via session.parent_id,
--     spawned by the Agent tool call (session_task_link/workflow_run/
--     workflow_activity are NOT populated here -- both live captures had
--     zero rows in those tables; see docs/zcode-adapter.md)
--   - model_usage rows: one with cache_read_input_tokens > 0 reproducing
--     the live-verified netInput = input_tokens - cache_read_input_tokens
--     arithmetic (14276 - 11712 = 2564, the exact foreign-mount sample
--     from the 2026-08-25 live re-parse), one on the child session
--     reproducing the native-WSL sample (12860 - 8448 = 4412), and one
--     usage-only row with assistant_message_id NULL (a session-title-
--     generation call) proving the strict-superset relationship over
--     message.data.tokens
--   - todo rows

PRAGMA foreign_keys = ON;

INSERT INTO schema_migration (id, checksum, app_version, time_applied) VALUES
  ('0001_init', 'deadbeefcafef00d0000000000000000000000000000000000000000000000', '0.16.3', 1756000000000);

-- ---------------------------------------------------------------------
-- Sessions: one interactive parent, one subagent child (parent_id set).
-- ---------------------------------------------------------------------

INSERT INTO session (
  id, project_id, workspace_id, parent_id, slug, directory, path, title,
  version, share_url, summary_additions, summary_deletions, summary_files,
  summary_diffs, revert, permission, time_created, time_updated,
  time_compacting, time_archived, task_type, title_source, title_message_id,
  time_title_updated, trace_id
) VALUES (
  'ses_a1b2c3d4e5f6', 'proj_fixture0001', 'ws_fixture0001', NULL,
  'fix-flaky-retry-test', '/home/dev/example-project', NULL,
  'Fix the flaky retry test in cache_test.go', '0.16.3', NULL,
  4, 2, 2, NULL, NULL, NULL,
  1756000000000, 1756000045000, NULL, NULL,
  'interactive', 'generated', 'msg_u0001', 1756000002200, 'trace_fixture0001'
);

INSERT INTO session (
  id, project_id, workspace_id, parent_id, slug, directory, path, title,
  version, share_url, summary_additions, summary_deletions, summary_files,
  summary_diffs, revert, permission, time_created, time_updated,
  time_compacting, time_archived, task_type, title_source, title_message_id,
  time_title_updated, trace_id
) VALUES (
  'ses_e5f6a7b8c9d0', 'proj_fixture0001', 'ws_fixture0001', 'ses_a1b2c3d4e5f6',
  'investigate-test-flakiness', '/home/dev/example-project', NULL,
  'Investigate test flakiness', '0.16.3', NULL,
  0, 0, 0, NULL, NULL, NULL,
  1756000015500, 1756000025000, NULL, NULL,
  'subagent', 'generated', 'msg_uc001', 1756000015600, 'trace_fixture0001'
);

-- ---------------------------------------------------------------------
-- Parent session messages
-- ---------------------------------------------------------------------

INSERT INTO message (id, session_id, time_created, time_updated, data, sequence) VALUES
  ('msg_u0001', 'ses_a1b2c3d4e5f6', 1756000001000, 1756000001000,
   '{"role":"user","path":{"cwd":"/home/dev/example-project"},"time":{"created":1756000001000}}',
   0);

INSERT INTO message (id, session_id, time_created, time_updated, data, sequence) VALUES
  ('msg_a0001', 'ses_a1b2c3d4e5f6', 1756000002200, 1756000045000,
   '{"role":"assistant","agent":"build","model":{"providerID":"zai","modelID":"glm-5.2"},"modelID":"glm-5.2","providerID":"zai","path":{"cwd":"/home/dev/example-project"},"time":{"created":1756000002200,"completed":1756000045000},"finish":"stop","variant":"medium","tokens":{"input":2564,"output":812,"reasoning":120,"cache":{"read":11712,"write":0}},"cost":0.0041}',
   1);

-- ---------------------------------------------------------------------
-- Parts under msg_u0001 (the user prompt)
-- ---------------------------------------------------------------------

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_text_u0001', 'msg_u0001', 'ses_a1b2c3d4e5f6', 1756000001000, 1756000001000,
   '{"type":"text","text":"The retry test in internal/cache/cache_test.go fails intermittently under -race. Can you find and fix it?"}',
   0);

-- ---------------------------------------------------------------------
-- Parts under msg_a0001 (the assistant turn) -- exercises every
-- observed live part type, plus the two silently-dropped ones
-- (step-start, timeline).
-- ---------------------------------------------------------------------

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_stepstart_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000002200, 1756000002200,
   '{"type":"step-start"}',
   0);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_reasoning_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000002500, 1756000006000,
   '{"type":"reasoning","text":"Let me read the failing test first, then reproduce it under -race before touching anything.","time":{"start":1756000002500,"end":1756000006000}}',
   1);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_read_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000006500, 1756000007200,
   '{"type":"tool","tool":"Read","callID":"call_read_0001","state":{"status":"completed","input":{"filePath":"internal/cache/cache_test.go"},"output":"func TestRetry(t *testing.T) {\n  // ... test body ...\n}\n","metadata":{"filepath":"internal/cache/cache_test.go","truncated":false},"title":"Read cache_test.go","time":{"start":1756000006500,"end":1756000007200}}}',
   2);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_bash_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000007500, 1756000009800,
   '{"type":"tool","tool":"Bash","callID":"call_bash_0001","state":{"status":"error","input":{"command":"go test ./internal/cache/... -run TestRetry -race -count=5"},"output":"--- FAIL: TestRetry (0.02s)\n    cache_test.go:88: retry count mismatch: got 2, want 3\nFAIL","metadata":{"output":"--- FAIL: TestRetry (0.02s)\n    cache_test.go:88: retry count mismatch: got 2, want 3\nFAIL","exit":1,"description":"Run the flaky retry test under -race","truncated":false},"title":"Run go test -race -count=5","time":{"start":1756000007500,"end":1756000009800}}}',
   3);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_stepfinish_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000009900, 1756000009900,
   '{"type":"step-finish","reason":"tool-calls","tokens":{"input":1180,"output":64,"reasoning":40,"total":1284,"cache":{"read":900,"write":0}},"cost":0.0006}',
   4);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_edit_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000010200, 1756000011000,
   '{"type":"tool","tool":"Edit","callID":"call_edit_0001","state":{"status":"completed","input":{"filePath":"internal/cache/cache_test.go"},"output":"applied","metadata":{"filepath":"internal/cache/cache_test.go","truncated":false},"title":"Edit cache_test.go: inject a fake clock","time":{"start":1756000010200,"end":1756000011000}}}',
   5);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_write_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000011200, 1756000011900,
   '{"type":"tool","tool":"Write","callID":"call_write_0001","state":{"status":"completed","input":{"filePath":"internal/cache/retry_helper_test.go"},"output":"created","metadata":{"filepath":"internal/cache/retry_helper_test.go","truncated":false},"title":"Write retry_helper_test.go","time":{"start":1756000011200,"end":1756000011900}}}',
   6);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_websearch_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000012200, 1756000014500,
   '{"type":"tool","tool":"WebSearch","callID":"call_search_0001","state":{"status":"completed","input":{"query":"golang deterministic clock injection test retry backoff"},"output":"3 results summarized.","metadata":{"truncated":false},"title":"Search: deterministic clock injection patterns","time":{"start":1756000012200,"end":1756000014500}}}',
   7);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_agent_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000015000, 1756000042000,
   '{"type":"tool","tool":"Agent","callID":"call_agent_0001","state":{"status":"completed","input":{"subagent_type":"investigate","prompt":"Investigate whether the flakiness is purely clock-related or also a genuine data race in the retry counter.","description":"Investigate test flakiness"},"output":"Subagent report: confirmed a genuine race on the retry counter (unsynchronized increment) in addition to the clock issue.","metadata":{"truncated":false},"title":"Investigate test flakiness","time":{"start":1756000015000,"end":1756000042000}}}',
   8);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_timeline_0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000042200, 1756000042200,
   '{"type":"timeline","timelineType":"model_change","from":{"providerID":"zai","modelID":"glm-5-turbo"},"to":{"providerID":"zai","modelID":"glm-5.2"}}',
   9);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_text_a0001', 'msg_a0001', 'ses_a1b2c3d4e5f6', 1756000044000, 1756000044500,
   '{"type":"text","text":"Fixed: the retry counter increment was unsynchronized (a genuine data race) and the test also depended on wall-clock timing. Added a mutex around the counter and injected a fake clock in retry_helper_test.go. `go test -race -count=20` is now green."}',
   10);

-- ---------------------------------------------------------------------
-- Child (subagent) session -- spawned by part_tool_agent_0001 above.
-- session.parent_id links it back to ses_a1b2c3d4e5f6, matching the
-- live-observed behavior; nothing in session_task_link/workflow_run/
-- workflow_activity is populated (those tables are empty in both live
-- captures this fixture is grounded against, so they are omitted here
-- too -- see docs/zcode-adapter.md "Known gaps").
-- ---------------------------------------------------------------------

INSERT INTO message (id, session_id, time_created, time_updated, data, sequence) VALUES
  ('msg_uc001', 'ses_e5f6a7b8c9d0', 1756000015600, 1756000015600,
   '{"role":"user","path":{"cwd":"/home/dev/example-project"},"time":{"created":1756000015600}}',
   0);

INSERT INTO message (id, session_id, time_created, time_updated, data, sequence) VALUES
  ('msg_ac001', 'ses_e5f6a7b8c9d0', 1756000016000, 1756000025000,
   '{"role":"assistant","agent":"investigate","model":{"providerID":"zai","modelID":"glm-5-turbo"},"modelID":"glm-5-turbo","providerID":"zai","path":{"cwd":"/home/dev/example-project"},"time":{"created":1756000016000,"completed":1756000025000},"finish":"stop","variant":"low","tokens":{"input":4412,"output":430,"reasoning":0,"cache":{"read":8448,"write":256}},"cost":0.0009}',
   1);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_text_uc001', 'msg_uc001', 'ses_e5f6a7b8c9d0', 1756000015600, 1756000015600,
   '{"type":"text","text":"Investigate whether the flakiness is purely clock-related or also a genuine data race in the retry counter."}',
   0);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_read_c001', 'msg_ac001', 'ses_e5f6a7b8c9d0', 1756000016200, 1756000016700,
   '{"type":"tool","tool":"Read","callID":"call_readc_0001","state":{"status":"completed","input":{"filePath":"internal/cache/retry.go"},"output":"func (r *retrier) increment() { r.count++ }\n","metadata":{"filepath":"internal/cache/retry.go","truncated":false},"title":"Read retry.go","time":{"start":1756000016200,"end":1756000016700}}}',
   1);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_tool_bash_c001', 'msg_ac001', 'ses_e5f6a7b8c9d0', 1756000017000, 1756000017400,
   '{"type":"tool","tool":"Bash","callID":"call_bashc_0001","state":{"status":"completed","input":{"command":"go test ./internal/cache/... -race -run TestRetry -count=20"},"output":"PASS","metadata":{"output":"PASS","exit":0,"description":"Confirm the race with the race detector","truncated":false},"title":"Run go test -race -count=20","time":{"start":1756000017000,"end":1756000017400}}}',
   2);

INSERT INTO part (id, message_id, session_id, time_created, time_updated, data, sequence) VALUES
  ('part_text_ac001', 'msg_ac001', 'ses_e5f6a7b8c9d0', 1756000024500, 1756000025000,
   '{"type":"text","text":"Confirmed: r.count++ in retry.go is an unsynchronized read-modify-write, a genuine data race under concurrent retries -- not just a clock-timing artifact."}',
   3);

-- ---------------------------------------------------------------------
-- Todos (parent session)
-- ---------------------------------------------------------------------

INSERT INTO todo (session_id, content, status, priority, position, time_created, time_updated) VALUES
  ('ses_a1b2c3d4e5f6', 'Reproduce the flaky failure under -race', 'completed', 'high', 0, 1756000002000, 1756000009900);

INSERT INTO todo (session_id, content, status, priority, position, time_created, time_updated) VALUES
  ('ses_a1b2c3d4e5f6', 'Add deterministic clock injection + fix the counter race', 'completed', 'medium', 1, 1756000002000, 1756000011900);

INSERT INTO todo (session_id, content, status, priority, position, time_created, time_updated) VALUES
  ('ses_a1b2c3d4e5f6', 'Verify with go test -race -count=20', 'pending', 'low', 2, 1756000002000, 1756000042000);

-- ---------------------------------------------------------------------
-- model_usage: the adapter's token source. Three rows:
--   mu_0001 -- the parent turn, cache_read_input_tokens > 0, reproducing
--             the live 2026-08-25 foreign-mount netting sample exactly
--             (14276 - 11712 = 2564).
--   mu_0002 -- the child (subagent) turn, reproducing the live
--             2026-08-25 native-WSL netting sample exactly
--             (12860 - 8448 = 4412).
--   mu_0003 -- a usage-only row (assistant_message_id IS NULL,
--             query_source='session_title') proving the strict-superset
--             relationship over message.data.tokens: this call has no
--             corresponding message row at all.
-- ---------------------------------------------------------------------

INSERT INTO model_usage (
  id, logical_request_id, attempt_index, session_id, turn_id, trace_id, span_id,
  assistant_message_id, parent_user_message_id, query_source, provider_id, model_id,
  variant, agent, mode, task_type, status, started_at, first_token_at, completed_at,
  duration_ms, time_to_first_token_ms, finish_reason, tool_call_count,
  input_tokens, output_tokens, reasoning_tokens, cache_creation_input_tokens,
  cache_read_input_tokens, provider_total_tokens, computed_total_tokens,
  retry_count, retryable, cancelled_by_user, context_exceeded,
  error_type, error_code, error_message, raw_usage_json, provider_metadata_json
) VALUES (
  'mu_0001', 'lrq_fixture0001', 0, 'ses_a1b2c3d4e5f6', 'turn_0001', 'trace_fixture0001', 'span_0001',
  'msg_a0001', 'msg_u0001', 'chat', 'zai', 'glm-5.2',
  'medium', 'build', 'chat', 'interactive', 'completed', 1756000002200, 1756000002900, 1756000045000,
  42800, 700, 'stop', 6,
  14276, 812, 120, 0,
  11712, 14988, 14988,
  0, 0, 0, 0,
  NULL, NULL, NULL, NULL, NULL
);

INSERT INTO model_usage (
  id, logical_request_id, attempt_index, session_id, turn_id, trace_id, span_id,
  assistant_message_id, parent_user_message_id, query_source, provider_id, model_id,
  variant, agent, mode, task_type, status, started_at, first_token_at, completed_at,
  duration_ms, time_to_first_token_ms, finish_reason, tool_call_count,
  input_tokens, output_tokens, reasoning_tokens, cache_creation_input_tokens,
  cache_read_input_tokens, provider_total_tokens, computed_total_tokens,
  retry_count, retryable, cancelled_by_user, context_exceeded,
  error_type, error_code, error_message, raw_usage_json, provider_metadata_json
) VALUES (
  'mu_0002', 'lrq_fixture0002', 0, 'ses_e5f6a7b8c9d0', 'turn_0002', 'trace_fixture0001', 'span_0002',
  'msg_ac001', 'msg_uc001', 'chat', 'zai', 'glm-5-turbo',
  'low', 'investigate', 'chat', 'subagent', 'completed', 1756000016000, 1756000016400, 1756000025000,
  9000, 400, 'stop', 2,
  12860, 430, 0, 256,
  8448, 13290, 13290,
  0, 0, 0, 0,
  NULL, NULL, NULL, NULL, NULL
);

INSERT INTO model_usage (
  id, logical_request_id, attempt_index, session_id, turn_id, trace_id, span_id,
  assistant_message_id, parent_user_message_id, query_source, provider_id, model_id,
  variant, agent, mode, task_type, status, started_at, first_token_at, completed_at,
  duration_ms, time_to_first_token_ms, finish_reason, tool_call_count,
  input_tokens, output_tokens, reasoning_tokens, cache_creation_input_tokens,
  cache_read_input_tokens, provider_total_tokens, computed_total_tokens,
  retry_count, retryable, cancelled_by_user, context_exceeded,
  error_type, error_code, error_message, raw_usage_json, provider_metadata_json
) VALUES (
  'usage_model_session_title_a1b2c3d4', 'lrq_fixture0003', 0, 'ses_a1b2c3d4e5f6', NULL, 'trace_fixture0001', NULL,
  NULL, NULL, 'session_title', 'zai', 'glm-5-turbo',
  NULL, NULL, 'title', NULL, 'completed', 1756000000500, 1756000000700, 1756000001800,
  1300, 200, 'stop', 0,
  340, 18, 0, 0,
  0, 358, 358,
  0, 0, 0, 0,
  NULL, NULL, NULL, NULL, NULL
);
