import { test } from 'node:test';
import assert from 'node:assert/strict';
import { approveBody, newTokenBody } from '../src/lib/f/agents.ts';

test('approve body', () => {
  assert.deepEqual(approveBody(4, 'suggest'), { hours: 4, mode: 'suggest' });
  // A select can hand over a string.
  assert.deepEqual(approveBody('2', 'navigate'), { hours: 2, mode: 'navigate' });
  assert.equal(approveBody(20, 'navigate').hours, 12);
  assert.equal(approveBody('x', 'navigate').hours, 8);
});

test('new token keeps the grant mode and takes the chosen duration', () => {
  assert.deepEqual(newTokenBody({ mode: 'suggest' }, 2), { hours: 2, mode: 'suggest' });
  assert.deepEqual(newTokenBody({ mode: 'navigate' }, '12'), { hours: 12, mode: 'navigate' });
});
