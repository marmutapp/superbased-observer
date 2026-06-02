# Auto-update your instruction files

Observer learns patterns from your captured activity — hot files,
common commands, edit-then-test pairs, onboarding sequences,
correction rules derived from failure→recovery traces — and can
materialise them into your project's instruction files.

It works on three files, mapping to three CLI targets:

| File | `--target` |
|---|---|
| `CLAUDE.md` | `claude` |
| `AGENTS.md` | `agents` |
| `.cursorrules` | `cursor` |

When you open one of these files, **two CodeLenses** appear at the
top:

- **🔄 Refresh from Observer learnings** — runs
  `observer suggest --apply --project <root> --target <…>` and
  reloads the editor with the new content (preserves anything
  outside Observer's managed block).
- **👁 Preview suggestions** — runs the same command **without**
  `--apply` and opens the dry-run output as a new untitled markdown
  editor beside the original, so you can see exactly what would be
  written before committing to it.

### Best practice

1. **Preview first**, especially on your first run, to confirm the
   composed body matches your project conventions.
2. Iterate by editing `[intelligence.suggest]` in
   `~/.observer/config.toml` (controls what kinds of patterns get
   composed) and re-running Preview.
3. Once you trust it, `Refresh` becomes a one-click action you can
   run every few days.

### Manual block markers

Observer only touches content between its own marker block:

```markdown
<!-- BEGIN OBSERVER -->
…composed content…
<!-- END OBSERVER -->
```

Anything outside the markers is preserved verbatim, so your
hand-authored sections survive every refresh.
