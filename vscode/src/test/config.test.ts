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
    childModel: '',
    childEffort: '',
    taskBudget: -1,
    runBudget: -1,
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
    childModel: 'sonnet',
    childEffort: 'low',
    taskBudget: 2.5,
    runBudget: 10,
  });
  assert.deepEqual(seed, {
    model: 'opus',
    claudeBin: '/opt/claude',
    countdown: '20s',
    log: 'debug.log',
    maxLines: 25,
    planTools: ['Read', 'Grep'],
    useApiKey: true,
    childModel: 'sonnet',
    childEffort: 'low',
    taskBudget: 2.5,
    runBudget: 10,
  });
});

test('strings are trimmed, and whitespace-only means unset', () => {
  assert.deepEqual(buildConfigSeed({ model: '  opus  ', countdown: '   ' }), { model: 'opus' });
});

test('zero budgets are retained as the explicit unlimited opt-out', () => {
  assert.deepEqual(buildConfigSeed({ taskBudget: 0, runBudget: 0 }), { taskBudget: 0, runBudget: 0 });
});

test('renderConfigSeed writes two-space-indented JSON with a trailing newline', () => {
  assert.equal(renderConfigSeed({ model: 'opus' }), '{\n  "model": "opus"\n}\n');
});
