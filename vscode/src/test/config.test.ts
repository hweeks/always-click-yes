import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildConfigSeed, renderConfigSeed } from '../config';

test('an empty defaults object seeds an empty file', () => {
  assert.deepEqual(buildConfigSeed({}), {});
});

test('only values the user set make it into the seed', () => {
  const seed = buildConfigSeed({
    model: 'opus',
    claudeBin: '',
    countdown: '20s',
    log: '',
    maxLines: 0,
    planTools: [],
    useApiKey: false,
  });
  assert.deepEqual(seed, { model: 'opus', countdown: '20s' });
});

test('every field carries through when set', () => {
  const seed = buildConfigSeed({
    model: 'opus',
    claudeBin: '/opt/claude',
    countdown: '20s',
    log: 'debug.log',
    maxLines: 25,
    planTools: ['Read', 'Grep'],
    useApiKey: true,
  });
  assert.deepEqual(seed, {
    model: 'opus',
    claudeBin: '/opt/claude',
    countdown: '20s',
    log: 'debug.log',
    maxLines: 25,
    planTools: ['Read', 'Grep'],
    useApiKey: true,
  });
});

test('strings are trimmed, and whitespace-only means unset', () => {
  assert.deepEqual(buildConfigSeed({ model: '  opus  ', countdown: '   ' }), { model: 'opus' });
});

test('renderConfigSeed writes two-space-indented JSON with a trailing newline', () => {
  assert.equal(renderConfigSeed({ model: 'opus' }), '{\n  "model": "opus"\n}\n');
});
