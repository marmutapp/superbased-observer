# Pick a daemon mode

The extension can either **attach** to a daemon you already started
in a terminal, or **manage** the daemon's lifecycle on your behalf.
Pick one via the `observer.daemon.mode` setting:

| Mode | What happens |
|---|---|
| **`detect`** (default) | Attach only. Never spawn. If no daemon is running, the status bar shows "Observer idle" and you start one yourself with `observer start`. |
| **`managed`** | The extension spawns `observer start` on activation and kills it on shutdown. Refuses to spawn a second daemon when one is already running (safety rail). |
| **`auto`** | If a daemon is running, attach to it. Otherwise spawn one — same supervision as `managed`. |

### Recommendation

If you usually run `observer start` in a tmux pane or a separate
terminal, **leave the default `detect`**.

If you prefer the editor to be the source of truth for the daemon,
flip to **`auto`** — that gives you the safety rail of `managed`
without losing the ability to attach when you've started the daemon
yourself.

### Crash recovery

In `managed` and `auto` modes, the extension restarts the daemon
with backoff `[1 s, 2 s, 5 s]` if it exits unexpectedly. After
three failed restarts a notification offers **Open Output Channel**
/ **Retry**.
