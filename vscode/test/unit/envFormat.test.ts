// Unit tests for the shell-detection + env-var formatter.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import {
  ProxyEnv,
  detectShell,
  formatEnv,
  proxyEnvFor,
} from '../../src/terminal/envFormat';

describe('detectShell', () => {
  test('classifies POSIX shells as bash', () => {
    assert.equal(detectShell('/bin/bash'), 'bash');
    assert.equal(detectShell('/usr/bin/zsh'), 'bash');
    assert.equal(detectShell('/usr/local/bin/fish'), 'bash');
    assert.equal(detectShell('/bin/sh'), 'bash');
  });

  test('classifies PowerShell variants', () => {
    assert.equal(detectShell('C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe'), 'powershell');
    assert.equal(detectShell('powershell.exe'), 'powershell');
    assert.equal(detectShell('/usr/bin/pwsh'), 'powershell');
    assert.equal(detectShell('pwsh.exe'), 'powershell');
  });

  test('classifies cmd.exe', () => {
    assert.equal(detectShell('C:\\Windows\\System32\\cmd.exe'), 'cmd');
    assert.equal(detectShell('cmd.exe'), 'cmd');
  });

  test('falls back to bash on unknown / empty input', () => {
    assert.equal(detectShell(undefined), 'bash');
    assert.equal(detectShell(''), 'bash');
    assert.equal(detectShell('/usr/bin/exotic-shell'), 'bash');
  });

  test('is case-insensitive', () => {
    assert.equal(detectShell('PowerShell.EXE'), 'powershell');
    assert.equal(detectShell('CMD.EXE'), 'cmd');
  });
});

describe('proxyEnvFor', () => {
  test('builds the documented triple', () => {
    const env = proxyEnvFor(8820);
    assert.equal(env.ANTHROPIC_BASE_URL, 'http://127.0.0.1:8820');
    assert.equal(env.OPENAI_BASE_URL, 'http://127.0.0.1:8820/v1');
    assert.equal(env.ENABLE_TOOL_SEARCH, 'true');
  });

  test('respects a non-default port', () => {
    const env = proxyEnvFor(9999);
    assert.equal(env.ANTHROPIC_BASE_URL, 'http://127.0.0.1:9999');
    assert.equal(env.OPENAI_BASE_URL, 'http://127.0.0.1:9999/v1');
  });
});

describe('formatEnv', () => {
  const env: ProxyEnv = {
    ANTHROPIC_BASE_URL: 'http://127.0.0.1:8820',
    OPENAI_BASE_URL: 'http://127.0.0.1:8820/v1',
    ENABLE_TOOL_SEARCH: 'true',
  };

  test('bash uses export with && separators', () => {
    const out = formatEnv(env, 'bash');
    assert.equal(
      out,
      'export ANTHROPIC_BASE_URL=http://127.0.0.1:8820 && export OPENAI_BASE_URL=http://127.0.0.1:8820/v1 && export ENABLE_TOOL_SEARCH=true',
    );
  });

  test('PowerShell uses $env: with single-quoted values and ; separators', () => {
    const out = formatEnv(env, 'powershell');
    assert.match(out, /^\$env:ANTHROPIC_BASE_URL='http:\/\/127\.0\.0\.1:8820'/);
    assert.match(out, /; \$env:OPENAI_BASE_URL='http:\/\/127\.0\.0\.1:8820\/v1'/);
    assert.match(out, /; \$env:ENABLE_TOOL_SEARCH='true'$/);
  });

  test('cmd uses set with && separators (no quoting)', () => {
    const out = formatEnv(env, 'cmd');
    assert.equal(
      out,
      'set ANTHROPIC_BASE_URL=http://127.0.0.1:8820 && set OPENAI_BASE_URL=http://127.0.0.1:8820/v1 && set ENABLE_TOOL_SEARCH=true',
    );
  });
});
