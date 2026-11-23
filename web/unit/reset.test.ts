import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mainRewritten } from '../src/lib/f/reset.ts';

test('main rewritten by a forge reset', () => {
  const seed = { sha: 'e08763f', at: '2026-10-27T10:05:00Z' };
  const merged = { sha: 'a1b2c3d', at: '2026-10-29T09:00:00Z' };
  assert.equal(mainRewritten(merged, seed), true);
  assert.equal(mainRewritten(seed, merged), false);
  assert.equal(mainRewritten(seed, seed), false);
  assert.equal(mainRewritten(undefined, seed), false);
  assert.equal(mainRewritten({ sha: 'x' }, seed), false);
});
