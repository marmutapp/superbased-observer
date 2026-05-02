// Package claudecode implements the Adapter interface for Anthropic's Claude
// Code CLI. It parses JSONL session files from ~/.claude/projects/... and
// maps Claude Code tool names (Read/Write/Edit/Bash/Grep/Glob/WebSearch/
// WebFetch) to the normalized action taxonomy. See spec §4.1.
package claudecode
