// Package crossmount discovers extra $HOME-equivalent directories that
// observer should treat as candidate watch roots when an AI tool runs
// on the "other side" of a WSL2/Windows pair.
//
// Concretely:
//
//   - On WSL2 (any Linux host where /mnt/c/Users is statable), each
//     directory under /mnt/c/Users becomes a candidate Windows home.
//   - On Windows (any Windows host where \\wsl.localhost\ is enumerable),
//     each <distro>/home/<user> becomes a candidate Linux home.
//   - On macOS / pure Linux / pure Windows, ExtraHomes returns nil.
//
// Each candidate is paired with an OS tag so adapters can produce the
// correct subpaths (e.g. Copilot's per-OS workspaceStorage location)
// without conflating the host runtime's GOOS with the home's logical
// OS.
package crossmount
