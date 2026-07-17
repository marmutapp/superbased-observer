// Package oscrypt implements Chromium's "Safe Storage" pattern for
// retrieving and using AES keys protected by the host OS's keystore.
// The same pattern is used by Chrome, Brave, Slack, Discord, VS Code,
// Cursor, Windsurf, Antigravity, and most other Electron apps.
//
// Per-OS retrieval mechanism:
//
//   - macOS: Login Keychain entry "{AppName} Safe Storage" via
//     `security` CLI. Returns the raw key bytes.
//   - Linux native: libsecret (GNOME Keyring / KWallet via D-Bus).
//     Falls back to the literal string "peanuts" when no Secret
//     Service is running — this is a documented Chromium fallback.
//   - WSL2 Linux observing Windows-side data: PowerShell helper.
//     The harness invokes `powershell.exe` to read Local State,
//     base64-decode the encrypted_key, strip the "DPAPI" prefix, and
//     CryptUnprotectData the rest. Detection: /proc/sys/fs/binfmt_misc/
//     WSLInterop existence and Windows-shaped roots in the home
//     resolver.
//   - Windows: Local State JSON's os_crypt.encrypted_key, base64-
//     decoded, "DPAPI" prefix stripped, CryptUnprotectData via
//     billgraziano/dpapi.
//
// PBKDF2 derivation uses the Chromium defaults — salt="saltysalt",
// 1 iteration, 16-byte key — applied only when the direct-key
// candidate fails.
//
// DecryptCTR runs a try-loop matching the third-party
// arashz/antigravity_decryptor pattern: AES-{128,256}-CTR with skip
// offsets [0,1,2,4,8] and a caller-provided plaintext validator.
// Wrong-key decryption is silent at the cipher level — the validator
// (typically protowire.ValidatesAsProto) is the only signal.
package oscrypt
