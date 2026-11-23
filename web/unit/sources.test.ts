import { test } from 'node:test';
import assert from 'node:assert/strict';
import { sourceRoute, pathLine } from '../src/lib/f/sources.ts';

test('trailer and mr sources open the history', () => {
  assert.equal(sourceRoute({ kind: 'trailer', label: 'trailer c-2hd7vw', ref: 'c-2hd7vw' }), '/history?q=c-2hd7vw');
  assert.equal(sourceRoute({ kind: 'mr', label: 'MR !2', ref: '!2' }), '/history?q=Reviewed-in%3A+%212');
  assert.equal(
    sourceRoute({ kind: 'history', label: 'history 5749a49', ref: '5749a4974305012471330d39ecb142e1f0cce87b' }),
    '/history/5749a4974305012471330d39ecb142e1f0cce87b',
  );
});

test('files open in the change worktree, or main', () => {
  const s = { kind: 'file', label: 'cn-003.yaml', ref: 'control/inventory/cn-003.yaml:21' };
  assert.equal(sourceRoute(s, 'c-k7m2qz'), '/changes/c-k7m2qz/files?path=control%2Finventory%2Fcn-003.yaml&line=21');
  assert.equal(sourceRoute(s), '/files?path=control%2Finventory%2Fcn-003.yaml&line=21');
  assert.deepEqual(pathLine('a/b.yaml'), { path: 'a/b.yaml' });
});

test('kb, lab, change, impact, entity', () => {
  assert.equal(sourceRoute({ kind: 'kb', label: 'x', ref: 'kb/runbooks/add-node.md' }), '/kb/article?path=kb%2Frunbooks%2Fadd-node.md');
  assert.equal(sourceRoute({ kind: 'lab', label: 'run-3', ref: 'c-k7m2qz/run-3' }), '/labs/runs/c-k7m2qz%2Frun-3');
  assert.equal(sourceRoute({ kind: 'lab', label: 'r', ref: 'kb/labs/c-k7m2qz-run-1.md' }), '/kb/article?path=kb%2Flabs%2Fc-k7m2qz-run-1.md');
  assert.equal(sourceRoute({ kind: 'change', label: 'c', ref: 'c-p4x8ma' }), '/changes/c-p4x8ma');
  assert.equal(sourceRoute({ kind: 'impact', label: 'impact', ref: 'c-k7m2qz' }), '/changes/c-k7m2qz/review?tab=impact');
  assert.equal(sourceRoute({ kind: 'entity', label: 'cn-003', ref: 'Node/cn-003' }), '/assets/Node/cn-003');
  assert.equal(sourceRoute({ kind: 'entity', label: 'GC5A-6Z', ref: 'Rack/GC5A-6Z' }), '/assets/racks/GC5A-6Z');
  assert.equal(sourceRoute({ kind: 'mr', label: 'MR !9', ref: 'http://127.0.0.1:3000/platform/state/pulls/9' }), 'http://127.0.0.1:3000/platform/state/pulls/9');
  assert.equal(sourceRoute({ kind: 'search', label: 'x' }), null);
});
