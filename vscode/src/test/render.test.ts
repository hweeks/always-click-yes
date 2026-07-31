import assert from 'node:assert/strict';
import { test } from 'node:test';
import type { Ask, AskOption } from '../../webview/protocol';
import { askSelection } from '../../webview/render';

function ask(multiSelect: boolean, optionCount: number): Ask {
  const options: AskOption[] = Array.from({ length: optionCount }, (_, i) => ({
    label: `option ${i}`,
    description: '',
    selected: false,
  }));
  return {
    header: 'Storage',
    question: 'Where should it go?',
    index: 0,
    total: 1,
    multiSelect,
    cursor: 0,
    options,
    deadlineUnixMs: 0,
  };
}

test('a single-select question keeps only the first index', () => {
  assert.deepEqual(askSelection(ask(false, 3), [1]), [1]);
  // answerAsk refuses more than one option for a single-select question, so the
  // client never sends one — the lowest index wins after the sort.
  assert.deepEqual(askSelection(ask(false, 3), [2, 0]), [0]);
});

test('a multi-select question keeps every index, deduped and ascending', () => {
  assert.deepEqual(askSelection(ask(true, 4), [2, 0, 3]), [0, 2, 3]);
  assert.deepEqual(askSelection(ask(true, 4), [1, 1, 1]), [1]);
});

test('out-of-range, negative and non-integer indices are dropped', () => {
  const a = ask(true, 3);
  assert.deepEqual(askSelection(a, [3]), []);
  assert.deepEqual(askSelection(a, [-1]), []);
  assert.deepEqual(askSelection(a, [1.5]), []);
  assert.deepEqual(askSelection(a, [Number.NaN]), []);
  assert.deepEqual(askSelection(a, [Number.POSITIVE_INFINITY]), []);
  assert.deepEqual(askSelection(a, [-1, 0, 1.5, 2, 9]), [0, 2]);
  // …and dropping them all is what disables the submit button, rather than
  // sending an answer the server would refuse with "no option chosen".
  assert.equal(askSelection(a, [7, -2]).length, 0);
});

test('an empty input gives an empty answer', () => {
  assert.deepEqual(askSelection(ask(true, 3), []), []);
  assert.deepEqual(askSelection(ask(false, 3), []), []);
  // A question with no options at all cannot be answered either.
  assert.deepEqual(askSelection(ask(true, 0), [0]), []);
});
