import assert from 'node:assert/strict';
import { test } from 'node:test';
import * as path from 'path';
import { claudeExeNames, findClaude, prependDir, wellKnownDirs } from '../claude';

const never = (_p: string) => false;

test('windows gets the shim names the two installers write', () => {
  assert.deepEqual(claudeExeNames('darwin'), ['claude']);
  assert.deepEqual(claudeExeNames('linux'), ['claude']);
  assert.deepEqual(claudeExeNames('win32'), ['claude.exe', 'claude.cmd', 'claude.bat']);
});

test('wellKnownDirs skips entries whose base dir is unknown', () => {
  assert.deepEqual(wellKnownDirs('darwin', { home: '/Users/x' }), [
    path.join('/Users/x', '.local', 'bin'),
    path.join('/Users/x', '.claude', 'local'),
    '/opt/homebrew/bin',
    '/usr/local/bin',
  ]);
  assert.deepEqual(wellKnownDirs('linux', {}), ['/opt/homebrew/bin', '/usr/local/bin']);
  assert.deepEqual(wellKnownDirs('win32', { appData: 'C:\\AppData' }), [
    path.join('C:\\AppData', 'npm'),
  ]);
});

test('the configured .acy.json path wins and is trusted verbatim', () => {
  const r = findClaude({
    configPath: '/opt/tools/claude',
    settingPath: '/ignored',
    platform: 'darwin',
    envPath: '/usr/bin',
    home: '/Users/x',
    isFile: never, // even when the probe can't see it
  });
  assert.deepEqual(r, { path: '/opt/tools/claude', source: 'config' });
});

test('blank explicit paths are ignored, and the setting is next in line', () => {
  const r = findClaude({
    configPath: '  ',
    settingPath: '/opt/tools/claude',
    platform: 'linux',
    isFile: never,
  });
  assert.deepEqual(r, { path: '/opt/tools/claude', source: 'setting' });

  const none = findClaude({
    configPath: '',
    settingPath: '   ',
    platform: 'linux',
    isFile: never,
  });
  assert.equal(none, undefined);
});

test('PATH is scanned in order, before the well-known dirs', () => {
  const hit = path.join('/second', 'claude');
  const r = findClaude({
    platform: 'darwin',
    envPath: '/first::/second:/third',
    home: '/Users/x',
    isFile: (p) => p === hit || p === path.join('/third', 'claude') || p === '/usr/local/bin/claude',
  });
  assert.deepEqual(r, { path: hit, source: 'path' });
});

test('windows PATH splits on semicolons and finds the .cmd shim', () => {
  const hit = path.join('C:\\tools', 'claude.cmd');
  const r = findClaude({
    platform: 'win32',
    envPath: 'C:\\bin;C:\\tools',
    isFile: (p) => p === hit,
  });
  assert.deepEqual(r, { path: hit, source: 'path' });
});

test('a well-known install dir catches the PATH a GUI launch missed', () => {
  const hit = path.join('/Users/x', '.local', 'bin', 'claude');
  const r = findClaude({
    platform: 'darwin',
    envPath: '/usr/bin:/bin',
    home: '/Users/x',
    isFile: (p) => p === hit,
  });
  assert.deepEqual(r, { path: hit, source: 'wellKnown' });
});

test('nothing anywhere is undefined, not a guess', () => {
  const r = findClaude({
    platform: 'linux',
    envPath: '/usr/bin',
    home: '/home/x',
    isFile: never,
  });
  assert.equal(r, undefined);
});

test('prependDir puts a dir first but never duplicates one already there', () => {
  assert.equal(prependDir('/usr/bin:/bin', '/opt/homebrew/bin', 'darwin'), '/opt/homebrew/bin:/usr/bin:/bin');
  assert.equal(prependDir('/usr/bin:/opt/homebrew/bin', '/opt/homebrew/bin', 'darwin'), '/usr/bin:/opt/homebrew/bin');
  assert.equal(prependDir(undefined, '/opt/homebrew/bin', 'darwin'), '/opt/homebrew/bin');
  assert.equal(prependDir('C:\\bin', 'C:\\tools', 'win32'), 'C:\\tools;C:\\bin');
});
