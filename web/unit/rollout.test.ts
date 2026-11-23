import { test } from 'node:test';
import assert from 'node:assert/strict';
import { rolloutSteps } from '../src/routes/changes/rollout.ts';
import { mergeParts, parseMessage } from '../src/routes/changes/trailers.ts';
import type { Change, Cluster, ClusterRollout } from '../src/lib/api/types.ts';

const SHA = '594dc29aa11b2c3d4e5f60718293a4b5c6d7e8f9';
const kust = (name: string, ready: boolean, revision = `main@sha1:${SHA}`) => ({ name, ready, revision });
const cluster = (extra: Record<string, unknown>) => ({ name: 'defiant', kustomizations: [], ...extra }) as unknown as Cluster;
const roll = (extra: Partial<ClusterRollout> = {}): ClusterRollout => ({ cluster: 'defiant', deployed: false, verified: false, ...extra });
const statuses = (r: ClusterRollout, cl?: Cluster) => rolloutSteps(r, cl, SHA).map((s) => s.status);

test('rollout: waits for Flux to fetch the merge commit', () => {
  const steps = rolloutSteps(roll(), cluster({ applied: { sha: 'e08763f' } }), SHA);
  assert.deepEqual(
    steps.map((s) => s.status),
    ['now', 'todo', 'todo', 'todo'],
  );
  assert.equal(steps[0].title, 'Flux fetched main@594dc29');
  assert.equal(steps[0].detail, 'waiting for the next poll');
});

test('rollout: applied, health checks running', () => {
  const cl = cluster({ objects: 23, applied: { sha: SHA }, kustomizations: [kust('flux-system', true), kust('cilium', false)] });
  const steps = rolloutSteps(roll({ deployed: true, sha: SHA }), cl, SHA);
  assert.deepEqual(
    steps.map((s) => s.status),
    ['done', 'done', 'now', 'todo'],
  );
  assert.equal(steps[1].title, 'Applied 23 objects');
  assert.equal(steps[2].title, 'Kustomizations 1 of 2 ready');
  assert.equal(steps[2].detail, 'health checks running');
});

test('rollout: verified counts every Kustomization ready', () => {
  const cl = cluster({ objects: 23, applied: { sha: SHA }, kustomizations: [kust('a', true), kust('b', true, 'old')] });
  const steps = rolloutSteps(roll({ deployed: true, verified: true, sha: SHA }), cl, SHA);
  assert.deepEqual(
    steps.map((s) => s.status),
    ['done', 'done', 'done', 'done'],
  );
  assert.equal(steps[2].title, 'Kustomizations 2 of 2 ready');
  assert.equal(steps[3].title, 'Verified: health checks pass');
});

test('rollout: a failing cluster marks the health step', () => {
  const cl = cluster({ applied: { sha: SHA }, healthy: false, failing: true, message: 'podinfo not ready' });
  const s = rolloutSteps(roll({ deployed: true, sha: SHA }), cl, SHA);
  assert.deepEqual(
    s.map((x) => x.status),
    ['done', 'done', 'fail', 'todo'],
  );
  assert.equal(s[2].detail, 'podinfo not ready');
  assert.deepEqual(statuses(roll({ deployed: true, sha: SHA })), ['done', 'done', 'now', 'todo']);
});

test('parseMessage: subject, body and the trailer block', () => {
  const p = parseMessage('Merge change c-1: Title (!4)\n\nThe intent.\n\nPk-Change: c-1\nApproved-by: anna, jonas\n');
  assert.equal(p.subject, 'Merge change c-1: Title (!4)');
  assert.deepEqual(p.body, ['The intent.']);
  assert.deepEqual(p.trailers, [
    ['Pk-Change', 'c-1'],
    ['Approved-by', 'anna, jonas'],
  ]);
  // A last paragraph with prose is body, not trailers.
  assert.deepEqual(parseMessage('Subject\n\nNot: a trailer\nplain line').trailers, []);
});

test('mergeParts: approvals pending until they arrive, no Pk-Title', () => {
  const c = {
    id: 'c-1',
    title: 'Add cn-035',
    intent: 'two nodes',
    author: 'dennis',
    workItems: ['w-114'],
    labRuns: [{ run: 'c-1/run-2', result: 'pass', report: 'kb/labs/c-1-run-2.md' }],
    mr: { number: 14 },
    approvalsBy: [],
  } as unknown as Change;
  const p = mergeParts(c);
  assert.equal(p.subject, 'Merge change c-1: Add cn-035 (!14)');
  const keys = p.trailers.map(([k]) => k);
  assert.ok(!keys.includes('Pk-Title'));
  assert.deepEqual(p.trailers.find(([k]) => k === 'Approved-by'), ['Approved-by', '(none yet)']);
  assert.deepEqual(p.trailers.find(([k]) => k === 'Pk-Lab-Run'), ['Pk-Lab-Run', 'c-1/run-2 pass kb/labs/c-1-run-2.md']);
  assert.deepEqual(p.trailers.at(-1), ['Reviewed-in', '!14']);
  const approved = mergeParts({ ...c, approvalsBy: [{ user: 'anna', counts: true }, { user: 'bot', counts: false }] } as unknown as Change);
  assert.deepEqual(approved.trailers.find(([k]) => k === 'Approved-by'), ['Approved-by', 'anna']);
});
