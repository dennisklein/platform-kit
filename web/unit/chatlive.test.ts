import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  answeredOnRequest,
  applyDelta,
  applyStep,
  closeMarkdown,
  elapsed,
  footLabel,
  modelCallFailed,
  newRequestId,
  offerData,
  stopSteps,
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

test('a slow model answer offers the data answer after 8 s, without text', () => {
  const l = { text: '', startedAt: 0 };
  assert.equal(offerData(l, 7999, true), false);
  assert.equal(offerData(l, 8000, true), true);
  // Built-in answers come fast; text means the model is writing.
  assert.equal(offerData(l, 9000, false), false);
  assert.equal(offerData({ ...l, text: 'c-2hd7vw' }, 9000, true), false);
});

test('a stopped answer keeps its steps; what ran is stopped', () => {
  const s = stopSteps([{ ...run(1, 'history cn-003'), state: 'ok', summary: '4 merges' }, run(2, "search 'port 37'")]);
  assert.deepEqual(
    s.map((x) => [x.state, x.summary]),
    [
      ['ok', '4 merges'],
      ['failed', 'stopped'],
    ],
  );
});

test('model line: secret guard, short built-in label in the side panel', () => {
  const builtin = { text: 'built-in · from project data', title: 't', fallback: false };
  assert.equal(footLabel(builtin, { model: 'guard', mock: true }, false).text, 'built-in · secret guard');
  assert.equal(footLabel(builtin, { model: 'guard' }, true).text, 'built-in · secret guard');
  assert.equal(footLabel(builtin, { model: 'mock', mock: true }, true).text, 'built-in');
  assert.equal(footLabel(builtin, { model: 'mock', mock: true }, false).text, 'built-in · from project data');
  const fb = { text: 'from project data', title: 't', fallback: true };
  assert.equal(footLabel(fb, { model: 'qwen2.5:7b', fallback: true }, true).text, 'built-in');
  const model = { text: 'qwen2.5:7b', title: 'Model', fallback: false };
  assert.equal(footLabel(model, { model: 'qwen2.5:7b' }, true).text, 'qwen2.5:7b');
});

test('llm log: an answer from project data on request is no failed model call', () => {
  const onRequest = { model: 'project-data' };
  assert.equal(answeredOnRequest(onRequest), true);
  assert.equal(modelCallFailed(onRequest), false);
  // Older entries recorded it as an error of the model call.
  const legacy = { model: 'qwen2.5:7b', error: 'answered from project data on request (no model call)' };
  assert.equal(answeredOnRequest(legacy), true);
  assert.equal(modelCallFailed(legacy), false);
  const slow = { model: 'qwen2.5:7b', error: 'model too slow (25s): context deadline exceeded' };
  assert.equal(answeredOnRequest(slow), false);
  assert.equal(modelCallFailed(slow), true);
  assert.equal(modelCallFailed({ model: 'qwen2.5:7b' }), false);
});
