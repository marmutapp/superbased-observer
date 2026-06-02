# Verify your install

`Observer: Doctor` runs the binary's own health-check against the
resolved observer install:

- DB integrity (`PRAGMA quick_check`)
- Hook integrity (registered hooks still match the recorded
  checksums)
- MCP registrations (each AI tool's config points at the right
  observer binary)
- The running binary's path against what `observer init` recorded

If something is misaligned the Doctor exits non-zero and prints
the offending check in the terminal — you'll see it before the
extension surfaces any false-positive errors elsewhere.

### What you should see

A new terminal opens with a header like:

```
$ /home/you/.local/bin/observer doctor
✓ DB integrity OK
✓ Hook integrity OK (3 hooks registered)
✓ MCP registrations OK (2 tools wired)
✓ Binary path matches init record
```

If you get `binary not found`, set `observer.binary.path` in your
Settings to an absolute path and re-run.
