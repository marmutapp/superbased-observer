// Package zcode implements a SQLite adapter for the zcode CLI's session
// store at ~/.zcode/cli/db/db.sqlite.
//
// zcode is an OpenCode fork: the session/message/part triple, the
// message.data / part.data JSON shapes, millisecond timestamps, and
// session.directory-as-cwd all match OpenCode, so this adapter is a
// structural transposition of internal/adapter/opencode for sessions,
// user prompts, tool calls, assistant text, reasoning threading,
// step-finish, subtasks, and todos.
//
// # The one divergence: tokens come from model_usage
//
// zcode adds a dedicated model_usage table (per-model-call rows carrying
// provider_id, model_id, a full input/output/reasoning/cache_read/
// cache_creation split, and raw_usage_json) alongside the per-message token
// bundle (message.data.tokens) base OpenCode already carries. Re-verified
// live 2026-08-25 (zcode-app-cli 3.8.1-15 / zcode-runtime 0.16.3, both a
// native WSL install and a foreign-mount Windows install): message.data.
// tokens is POPULATED for every completed assistant message and matches its
// model_usage row's split exactly — it is not zeroed. loadTokenEvents still
// reads model_usage rather than the message JSON because model_usage is a
// strict superset: it also carries usage-only calls with no corresponding
// message row at all (e.g. a builtin session-title-generation call keyed
// "usage_model_session_title_...", assistant_message_id NULL), which
// message.data.tokens can never see. Sourcing from a single table also keeps
// token capture single-seamed instead of split across two paths. model_usage
// .input_tokens is GROSS (includes cache_read, OpenAI-style), so it is
// netted against cache_read to fill Observer's NET TokenEvent.InputTokens
// (feedback_openai_input_is_gross).
//
// # Deferred
//
// §14.3 Tier-2 cache observation is intentionally not emitted in v1. It was
// originally deferred on the (now corrected) premise that the per-message
// token bundle was zero; since message.data.tokens is in fact populated, a
// follow-up COULD derive Tier-2 blocks from it (or from model_usage's
// cache_read/cache_creation columns) the same way OpenCode's cachetrack path
// does. That build-out is still open — see
// docs/plans/zcode-adapter-plan-2026-08-18.md and the "Known gaps" section
// of docs/zcode-adapter.md.
package zcode
