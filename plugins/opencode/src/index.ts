// SuperBased Observer — OpenCode plugin.
//
// HAND-WRITTEN GLUE. It carries no wiring of its own: the MCP entry it
// installs lives in ./wiring.generated.ts, which plugins/plugingen produces
// by running observer's REAL MCP registrar (internal/mcp) against a sandbox
// HOME and reading back what it wrote. Change the registrar, run
// `make plugins-build`, and this plugin follows automatically.
//
// The seam is OpenCode's `config` hook — `config?: (input: Config) =>
// Promise<void>` in @opencode-ai/plugin's Hooks — which receives the merged
// config object and mutates it in place. Adding our server there means the
// user never edits opencode.json's `mcp` block by hand.

import type { Plugin } from "@opencode-ai/plugin";

import {
  OBSERVER_MCP_SERVER,
  OBSERVER_MCP_SERVER_NAME,
} from "./wiring.generated.js";

/**
 * Registers the local SuperBased Observer MCP server with OpenCode.
 *
 * Requires the `observer` binary on PATH (`npm i -g @superbased/observer` or
 * `pipx install superbased-observer`). This plugin installs no binary.
 */
export const ObserverPlugin: Plugin = async () => {
  return {
    config: async (config) => {
      const mcp = (config.mcp ??= {});
      // Never double-register. `observer init --opencode` writes this same
      // server under this same key, and a user may also have added it by
      // hand — in either case the existing entry is authoritative and we
      // leave it alone, so carrying both the plugin and init's write
      // declares the server exactly once.
      if (mcp[OBSERVER_MCP_SERVER_NAME] !== undefined) return;
      mcp[OBSERVER_MCP_SERVER_NAME] = { ...OBSERVER_MCP_SERVER };
    },
  };
};

export default ObserverPlugin;
