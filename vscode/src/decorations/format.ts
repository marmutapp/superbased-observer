// Pure formatters for the file-freshness decoration + hover. Lives
// in its own module so the unit suite can pin the strings.

import type { FileStateResponse } from '../api/types';

/**
 * decorationBadge renders the explorer-strip badge. VS Code allows
 * up to 2 characters; we use a single bullet so the marker reads as
 * "recent activity" without competing with git decoration badges.
 *
 * Returns undefined when there's no observed activity in the window,
 * so the provider can return undefined and skip the decoration.
 */
export function decorationBadge(state: FileStateResponse): string | undefined {
  const touched =
    !!state.last_read_at ||
    (state.edit_count_24h ?? 0) > 0 ||
    (state.stale_rereads_24h ?? 0) > 0;
  return touched ? '•' : undefined;
}

/**
 * decorationTooltip builds the short, single-line decoration tooltip
 * shown when the user hovers the explorer badge. The Hover provider
 * uses a richer Markdown variant via hoverMarkdown.
 */
export function decorationTooltip(state: FileStateResponse): string {
  const parts: string[] = [];
  if (state.last_read_by) {
    parts.push(`last read by ${state.last_read_by}`);
  }
  if ((state.edit_count_24h ?? 0) > 0) {
    parts.push(`${state.edit_count_24h} edits / 24h`);
  }
  if ((state.stale_rereads_24h ?? 0) > 0) {
    parts.push(`${state.stale_rereads_24h} stale re-reads flagged`);
  }
  if ((state.tools_touched ?? []).length > 0) {
    parts.push(`tools: ${state.tools_touched.join(', ')}`);
  }
  return parts.length === 0 ? 'no recent activity' : parts.join(' · ');
}

/**
 * hoverMarkdown is the Markdown body shown by the HoverProvider when
 * the user hovers line 1 of the active file. Multi-line, themed via
 * the standard `**bold**` convention.
 */
export function hoverMarkdown(state: FileStateResponse): string {
  const lines = ['**Observer**'];
  if (state.last_read_at && state.last_read_by) {
    lines.push(
      `Last read by \`${state.last_read_by}\` at \`${state.last_read_at}\``,
    );
  }
  if ((state.edit_count_24h ?? 0) > 0) {
    lines.push(`**Edits (24h)**: ${state.edit_count_24h}`);
  }
  if ((state.stale_rereads_24h ?? 0) > 0) {
    lines.push(
      `**Stale re-reads (24h)**: ${state.stale_rereads_24h} — these are flagged, not prevented`,
    );
  }
  if ((state.tools_touched ?? []).length > 0) {
    lines.push(`**Tools touched (24h)**: ${state.tools_touched.join(', ')}`);
  }
  if (lines.length === 1) {
    lines.push('_No recent activity in the last 24 h._');
  }
  return lines.join('\n\n');
}
