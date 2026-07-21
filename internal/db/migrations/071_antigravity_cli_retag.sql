-- 071_antigravity_cli_retag.sql — retag historical Antigravity agy-CLI rows
-- (token_usage + actions + sessions) from tool='antigravity' to
-- tool='antigravity-cli'.
--
-- HISTORY. Commit 71bb58c2 ("fix(antigravity): tag agy-CLI rows
-- antigravity-cli, not antigravity") added a re-tag boundary in
-- internal/adapter/antigravity/adapter.go::ParseSessionFile: the CLI parse
-- helpers hardcode models.ToolAntigravity, and the NewCLI adapter re-tags
-- emitted events to Name()="antigravity-cli". Rows ingested BEFORE that
-- commit landed under tool='antigravity' even though they came from CLI
-- session files.
--
-- WHY A DATA MIGRATION IS NECESSARY. store.InsertTokenEvents' upsert now
-- guards `... DO UPDATE SET ... WHERE token_usage.tool = excluded.tool`
-- (a cross-tool key collision must not mutate another tool's row, so the
-- dedup-sweep batch-gates on hasClaudeCode/hasCodex stay exact). That makes
-- `observer backfill --antigravity-cli-rescan` — which re-emits the SAME
-- (source_file, source_event_id) keys under tool='antigravity-cli' — a no-op
-- against these mislabeled tool='antigravity' rows: their token/model
-- refinements would silently be dropped. And the cost engine filters token
-- rows through sessions.tool (internal/intelligence/cost/summary.go `s.tool =
-- ?`), so a token-only retag would make CLI-filtered views lose rows while
-- desktop views over-include — sessions (and actions) must retag too for
-- sessions proven CLI.
--
-- DISCRIMINATOR — the LAYOUT PATH, mirroring adapter.go::classifyLayout.
-- classifyLayout keys the CLI layout on the slash-normalized, lowercased
-- substring `/.gemini/antigravity-cli/conversations/` (desktop uses
-- `/.gemini/antigravity/conversations/`, and CLI is tested FIRST because the
-- desktop substring is a prefix of the CLI one). This predicate MUST stay in
-- sync with classifyLayout. Notes:
--   (a) REPLACE(source_file,'\','/') normalizes Windows / foreign backslash
--       paths, matching classifyLayout's slash normalization; SQLite LIKE is
--       ASCII case-insensitive, matching classifyLayout's ToLower.
--   (b) This subsumes the antigravity-cli-db: event-id prefix (clidb.go:87):
--       those rows' source_file is the .db under the same CLI dir. It ALSO
--       catches older LayoutCLI `.pb` token rows emitted under AMBIGUOUS
--       event-id namespaces (antigravity-struct-*) — the event id can't
--       discriminate those, but the path can.
--   (c) transcript-synthesized actions carry the sessionPath as source_file,
--       so a desktop overview.txt augmentation row lives under
--       …/.gemini/antigravity/conversations/… and does NOT match — the
--       pattern requires the `antigravity-cli` segment. (This is why the
--       antigravity-cli-transcript: EVENT-ID prefix, which desktop
--       augmentation also emits, is NOT used as the discriminator.)
--   (d) sessions retag by child-row evidence in EITHER table; the two EXISTS
--       clauses are order-independent and idempotent.
--
-- Node-local only: no wire-shape change, no server-side migration pair.
-- Idempotent: rows already at 'antigravity-cli' don't match tool='antigravity'.

UPDATE token_usage
   SET tool = 'antigravity-cli'
 WHERE tool = 'antigravity'
   AND REPLACE(source_file, '\', '/') LIKE '%/.gemini/antigravity-cli/conversations/%';

UPDATE actions
   SET tool = 'antigravity-cli'
 WHERE tool = 'antigravity'
   AND REPLACE(source_file, '\', '/') LIKE '%/.gemini/antigravity-cli/conversations/%';

UPDATE sessions
   SET tool = 'antigravity-cli'
 WHERE tool = 'antigravity'
   AND (EXISTS (SELECT 1 FROM token_usage t
                 WHERE t.session_id = sessions.id
                   AND REPLACE(t.source_file, '\', '/') LIKE '%/.gemini/antigravity-cli/conversations/%')
        OR EXISTS (SELECT 1 FROM actions a
                    WHERE a.session_id = sessions.id
                      AND REPLACE(a.source_file, '\', '/') LIKE '%/.gemini/antigravity-cli/conversations/%'));
