import { test } from 'node:test';
import assert from 'node:assert/strict';
import { lifecycleLabel, lifecycleState, lifecycleTone } from '../../src/lib/change/lifecycle.ts';
import { matches } from '../../src/lib/change/rail.ts';

test('lifecycleState: a passing lab on a draft reads "lab passed"', () => {
  const s = lifecycleState({ state: 'draft', labState: 'pass' });
  assert.equal(s, 'labpass');
  assert.equal(lifecycleLabel(s), 'lab passed');
  assert.equal(lifecycleTone(s), 'ok');
});

test('lifecycleState: lab failures and runs show on draft and lab', () => {
  assert.equal(lifecycleState({ state: 'draft', labState: 'fail' }), 'labfail');
  assert.equal(lifecycleState({ state: 'lab', labState: 'running' }), 'labrun');
  assert.equal(lifecycleState({ state: 'draft', labState: 'none' }), 'draft');
  assert.equal(lifecycleState({ state: 'draft' }), 'draft');
});

test('lifecycleState: after mark ready the change state wins', () => {
  assert.equal(lifecycleState({ state: 'review', labState: 'pass' }), 'review');
  assert.equal(lifecycleState({ state: 'approved', labState: 'pass' }), 'approved');
  assert.equal(lifecycleState({ state: 'verified', labState: 'pass' }), 'verified');
  assert.equal(lifecycleLabel('review'), 'review');
});

test('rail search finds "lab passed"', () => {
  const c = { id: 'c-aaaaaa', title: 'x', author: 'dennis', branch: 'change/x', state: 'draft', labState: 'pass' } as never;
  assert.ok(matches(c, 'lab passed'));
  assert.ok(!matches({ ...(c as object), labState: 'none' } as never, 'passed'));
});
