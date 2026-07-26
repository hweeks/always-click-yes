import assert from 'node:assert/strict';
import { test } from 'node:test';
import * as path from 'path';
import { bundledBinaryPath, exeName, findOnPath, needsChmod, resolveBinary, runArgs } from '../launch';

const never = (_p: string) => false;

test('exeName appends .exe only on windows', () => {
  assert.equal(exeName('darwin'), 'acy');
  assert.equal(exeName('linux'), 'acy');
  assert.equal(exeName('win32'), 'acy.exe');
});

test('an explicit setting wins and is trusted verbatim', () => {
  const r = resolveBinary({
    settingPath: '/opt/tools/acy',
    extensionRoot: '/ext',
    platform: 'darwin',
    envPath: '/usr/bin',
    isFile: never, // even when the probe can't see it
  });
  assert.deepEqual(r, { path: '/opt/tools/acy', source: 'setting' });
});

test('a blank setting is ignored', () => {
  const r = resolveBinary({
    settingPath: '  ',
    extensionRoot: '/ext',
    platform: 'linux',
    envPath: undefined,
    isFile: never,
  });
  assert.equal(r, undefined);
});

test('the bundled binary is found under the extension root', () => {
  const bundled = bundledBinaryPath('/ext', 'linux');
  assert.equal(bundled, path.join('/ext', 'bin', 'acy'));

  const r = resolveBinary({
    extensionRoot: '/ext',
    platform: 'linux',
    envPath: '/usr/bin',
    isFile: (p) => p === bundled,
  });
  assert.deepEqual(r, { path: bundled, source: 'bundled' });
});

test('PATH is the last resort, scanned in order', () => {
  const hit = path.join('/second', 'acy');
  const r = resolveBinary({
    extensionRoot: '/ext',
    platform: 'darwin',
    envPath: '/first:/second:/third',
    isFile: (p) => p === hit || p === path.join('/third', 'acy'),
  });
  assert.deepEqual(r, { path: hit, source: 'path' });
});

test('windows PATH uses semicolons and acy.exe', () => {
  const hit = path.join('C:\\tools', 'acy.exe');
  const found = findOnPath('C:\\bin;C:\\tools', 'win32', (p) => p === hit);
  assert.equal(found, hit);
});

test('empty PATH entries are skipped, and a miss is undefined', () => {
  assert.equal(findOnPath('::/nowhere:', 'linux', never), undefined);
  assert.equal(findOnPath(undefined, 'linux', never), undefined);
});

test('needsChmod fires only when no execute bit survives', () => {
  assert.equal(needsChmod(0o644), true);
  assert.equal(needsChmod(0o666), true);
  assert.equal(needsChmod(0o000), true);
  assert.equal(needsChmod(0o755), false);
  assert.equal(needsChmod(0o700), false); // owner-only is enough
  assert.equal(needsChmod(0o111), false);
});

test('needsChmod ignores the file-type bits a raw stat mode carries', () => {
  assert.equal(needsChmod(0o100755), false); // S_IFREG | 0755
  assert.equal(needsChmod(0o100644), true); // S_IFREG | 0644
});

test('runArgs carries only the invocation, never settings', () => {
  assert.deepEqual(runArgs(false), ['run']);
  assert.deepEqual(runArgs(true), ['run', '--continue']);
});
