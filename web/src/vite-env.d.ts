/// <reference types="vite/client" />

// vite/client declares the ambient asset modules Vite handles at build time
// (`*.css`, `*.svg`, …) plus import.meta.env typings. It is referenced here
// so TypeScript can type side-effect and dynamic imports of stylesheets —
// e.g. LaunchTerminal's lazy `import("@xterm/xterm/css/xterm.css")`.
