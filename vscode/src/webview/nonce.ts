// Per-render CSP nonce generator for the webview panel.
//
// 24 random bytes → 32 base64url characters → ~192 bits of entropy,
// well above the threshold any reasonable script-src policy requires.

import * as crypto from 'node:crypto';

const NONCE_BYTES = 24;

export function generateNonce(): string {
  return crypto.randomBytes(NONCE_BYTES).toString('base64url');
}

// Exposed so tests can pin the expected length without re-deriving
// the base64url math.
export const NONCE_LENGTH = Math.ceil((NONCE_BYTES * 4) / 3);
