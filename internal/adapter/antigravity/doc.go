// Package antigravity parses session data from Google Antigravity,
// the VS Code-fork IDE shipped alongside Gemini 3 (Nov 2025). It
// stores conversations as AES-encrypted Protocol Buffer files under
// ~/.gemini/antigravity/conversations/<uuid>.pb plus a SQLite-backed
// index in state.vscdb.
//
// The adapter is structurally novel for the project:
//
//   - First adapter that decrypts before parsing. Uses the
//     internal/platform/oscrypt package for OSCrypt key retrieval and
//     a multi-cipher try-loop (AES-CTR/CBC/GCM × multiple skip
//     offsets) to handle the macOS-documented format plus whatever
//     scheme Antigravity-on-Windows actually uses.
//   - First adapter whose primary on-disk format isn't text. Uses
//     internal/platform/protowire's wire-walk + content heuristics
//     to extract message records without committing to specific
//     field numbers (the .proto schema is undocumented).
//   - First multi-provider tool: agents in Antigravity can run
//     against Gemini, Claude, or GPT-OSS at the user's choice. Per-
//     message model extraction is load-bearing for cost engine
//     correctness.
//
// On WSL2 with Windows-side data, the oscrypt PowerShell helper
// fetches the OSCrypt key cross-OS — no native-Windows observer
// install needed, but only if powershell.exe is reachable.
//
// state.vscdb (and the sister state.vscdb.backup) provides the
// trajectorySummaries index used to enrich sessions with title +
// workspace URI. Read-only with shared SQLite locks; safe to read
// while Antigravity is running.
//
// Cursor strategy: file-size-based, like cline. Antigravity rewrites
// the entire .pb on every turn, so byte-offset cursoring doesn't
// apply. Re-decrypt on every size advance; idempotent dedup at the
// store layer via (source_file, source_event_id).
//
// Scope (v1):
//   - conversations/*.pb only.
//   - implicit/*.pb (similarly encrypted, 17 files in the user's
//     corpus) is deferred.
//   - annotations/*.pbtxt (plain-text protobuf) is deferred.
//   - brain/<uuid>/*.md task-mode artifacts are deferred.
package antigravity
