import { test } from 'node:test';
import assert from 'node:assert/strict';
import { clusterHealth, changeCounts, openBreakdown } from '../src/lib/f/clusters.ts';

test('cluster health', () => {
  const ok = { healthy: true, applied: { sha: 'a' }, verified: { sha: 'a' } };
  assert.deepEqual(clusterHealth(ok), { tone: 'ok', label: 'healthy' });
  assert.equal(clusterHealth({ ...ok, verified: { sha: 'b' } }).label, 'verifying');
  assert.equal(clusterHealth({ ...ok, pending: { sha: 'c' } }).label, 'reconciling');
  assert.equal(clusterHealth({ ...ok, held: true }).label, 'held');
  assert.equal(clusterHealth({ ...ok, failing: true }).tone, 'bad');
  assert.equal(clusterHealth({ ...ok, reachable: false }).tone, 'muted');
});

test('change counts for the landing tiles', () => {
  const n = changeCounts([
    { group: 'mine', labState: 'none' },
    { group: 'mine', labState: 'running' },
    { group: 'others', labState: 'fail' },
    { group: 'bots' },
    { group: 'review', labState: 'pass' },
  ]);
  assert.deepEqual(n, { open: 5, mine: 2, others: 1, review: 1, bots: 1, labFailed: 1, labRunning: 1 });
  assert.equal(openBreakdown(n), '2 mine · 1 to review · 1 by others · 1 by a bot');
  assert.equal(openBreakdown(changeCounts([{ group: 'mine' }])), '1 mine');
});
