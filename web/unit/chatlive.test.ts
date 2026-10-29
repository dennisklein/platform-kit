import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  applyDelta,
  applyStep,
  closeMarkdown,
  elapsed,
  newRequestId,
  stepText,
  usedTools,
  type LiveStep,
} from '../src/lib/components/chat/live.ts';

const run = (id: number, label: string): LiveStep => ({ id, tool: 'search', label, state: 'running' });

test('steps: running, then done; a late running event keeps the done state', () => {
  let s = applyStep([], run(1, "search 'port 37'"));
  s = applyStep(s, run(2, 'history cn-003'));
  s = applyStep(s, { ...run(1, "search 'port 37'"), state: 'ok', summary: '4 hits' });
  assert.deepEqual(
    s.map((x) => [x.id, x.state]),
    [
      [1, 'ok'],
      [2, 'running'],
    ],
  );
  s = applyStep(s, run(1, "search 'port 37'"));
  assert.equal(s[0].state, 'ok');
  // Out of order: step 2 before step 1.
  assert.deepEqual(
    applyStep([run(2, 'b')], run(1, 'a')).map((x) => x.id),
    [1, 2],
  );
});

test('deltas append, reset starts over', () => {
  let t = applyDelta('', { requestId: 'r', text: 'c-2hd7vw ' });
  t = applyDelta(t, { requestId: 'r', text: 'moved' });
  assert.equal(t, 'c-2hd7vw moved');
  assert.equal(applyDelta(t, { requestId: 'r', text: '', reset: true }), '');
  assert.equal(applyDelta(t, { requestId: 'r', text: 'From data', reset: true }), 'From data');
});

test('half-streamed Markdown renders closed', () => {
  assert.equal(closeMarkdown('the **overl'), 'the **overl**');
  assert.equal(closeMarkdown('the **'), 'the ');
  assert.equal(closeMarkdown('read `bgp-rack'), 'read `bgp-rack`');
  assert.equal(closeMarkdown('Run:\n\n```\npk change new'), 'Run:\n\n```\npk change new\n```');
  assert.equal(closeMarkdown('**done** and `ok`.'), '**done** and `ok`.');
});

test('labels', () => {
  assert.equal(stepText({ id: 1, tool: 'history', label: 'history cn-003', summary: '4 merges', state: 'ok' }), 'history cn-003 → 4 merges');
  assert.equal(stepText(run(1, 'read bgp-rack-6z.yaml')), 'read bgp-rack-6z.yaml');
  assert.equal(usedTools(1), 'Used 1 tool');
  assert.equal(usedTools(3), 'Used 3 tools');
  assert.equal(elapsed(0, 6400), '6 s');
  assert.equal(elapsed(0, 75000), '1 min 15 s');
  assert.notEqual(newRequestId(), newRequestId());
});
