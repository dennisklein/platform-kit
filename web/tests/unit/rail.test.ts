import { test } from 'node:test';
import assert from 'node:assert/strict';
import { railGroups, railHints, stackOrder, groupCounts } from '../../src/lib/change/rail.ts';
import { collapse, diffStats, lineDiff, parseUnified } from '../../src/lib/change/diff.ts';
import { duration, runLabel, shortSha, timeAgo, tildePath, changeIdOfBranch } from '../../src/lib/change/format.ts';

const base = {
  branch: 'change/x',
  target: 'main',
  authorKind: 'human',
  labState: 'none',
  local: false,
  dirty: false,
  aheadTarget: 1,
  behindTarget: 0,
  aheadRemote: 0,
  behindRemote: 0,
  pushed: true,
  files: 1,
  commits: 1,
  approvals: { have: 0, need: 1 },
} as const;

function ch(id: string, over: Record<string, unknown>) {
  return { ...base, id, title: id, author: 'dennis', state: 'draft', group: 'mine', ...over } as any;
}

const list = [
  ch('c-31d0aa', { title: 'Cabling for GC5A-6Z', state: 'approved', updatedAt: '2026-10-27T10:00:00Z' }),
  ch('c-k7m2qz', { title: 'Add cn-035', parent: 'c-31d0aa', state: 'lab', updatedAt: '2026-10-28T10:00:00Z', behindTarget: 3 }),
  ch('c-p4x8ma', { title: 'BGP peers', author: 'anna', group: 'others', state: 'lab', labState: 'fail' }),
  ch('c-6m1pqd', { title: 'Bump podinfo', author: 'jonas', group: 'review', state: 'review' }),
  ch('c-h3sy7n', { title: 'Sync serials', author: 'sync-bot', authorKind: 'bot', group: 'bots' }),
];

test('groups in rail order, stacked child after its parent', () => {
  const g = railGroups(list, 'all', 'updated');
  assert.deepEqual(g.map((x) => x.key), ['mine', 'others', 'review', 'bots']);
  assert.deepEqual(g[0].items.map((i) => [i.c.id, i.depth]), [['c-31d0aa', 0], ['c-k7m2qz', 1]]);
});

test('filter and search', () => {
  assert.deepEqual(railGroups(list, 'review', 'updated').map((g) => g.key), ['review']);
  const g = railGroups(list, 'all', 'updated', 'bgp');
  assert.equal(g.length, 1);
  assert.equal(g[0].items[0].c.id, 'c-p4x8ma');
  assert.equal(railGroups(list, 'all', 'updated', 'anna peers')[0].items[0].c.id, 'c-p4x8ma');
});

test('a child without its parent in view is depth 0', () => {
  const g = railGroups(list, 'all', 'updated', 'cn-035');
  assert.deepEqual(g[0].items.map((i) => i.depth), [0]);
});

test('stack order survives cycles', () => {
  const a = ch('a', { parent: 'b' });
  const b = ch('b', { parent: 'a' });
  assert.equal(stackOrder([a, b]).length, 2);
});

test('hints', () => {
  const h = railHints(list[1]).map((x) => x.text);
  assert.deepEqual(h, ['dennis', 'on c-31d0aa', 'behind c-31d0aa by 3']);
  const agent = railHints(ch('c', { agent: 'dennis-agent' })).map((x) => x.text);
  assert.equal(agent[0], 'dennis + agent');
  assert.equal(groupCounts(list).mine, 2);
});

test('unified diff parse', () => {
  const d = parseUnified('--- a/x\n+++ b/x\n@@ -20,3 +20,4 @@\n a\n-b\n+c\n+d\n e\n');
  assert.deepEqual(diffStats(d), { added: 2, deleted: 1 });
  const add = d.filter((l) => l.kind === 'add');
  assert.deepEqual(add.map((l) => l.new), [21, 22]);
  assert.equal(d.find((l) => l.kind === 'del')!.old, 21);
});

test('line diff and collapse', () => {
  const a = Array.from({ length: 20 }, (_, i) => `l${i}`).join('\n');
  const b = a.replace('l10', 'L10');
  const d = lineDiff(a, b);
  assert.deepEqual(diffStats(d), { added: 1, deleted: 1 });
  const c = collapse(d, 2);
  assert.equal(c.filter((x) => x.kind === 'gap').length, 2);
});

test('formatters', () => {
  assert.equal(duration(400_000), '6m 40s');
  assert.equal(duration(25_000), '25s');
  assert.equal(runLabel('c-k7m2qz/run-3'), 'run-3');
  assert.equal(shortSha('a41f2c9e0'), 'a41f2c9');
  assert.equal(timeAgo('2026-10-28T10:00:00Z', Date.parse('2026-10-28T10:05:00Z')), '5 min ago');
  assert.equal(tildePath('/home/dennis/pk/c-k7m2qz'), '~/pk/c-k7m2qz');
  assert.equal(changeIdOfBranch('change/c-31d0aa-cabling'), 'c-31d0aa');
});

test('recent own merges get their own group after my changes, without open hints', () => {
  const merged = ch('c-19f4tb', { title: 'Kernel args', state: 'deployed', behindTarget: 4, mergedAt: '2026-10-27T10:05:00Z' });
  const g = railGroups(list, 'all', 'updated', '', [merged]);
  assert.deepEqual(g.map((x) => x.key), ['mine', 'merged', 'others', 'review', 'bots']);
  assert.deepEqual(g[0].items.map((i) => i.c.id), ['c-31d0aa', 'c-k7m2qz']);
  assert.deepEqual(g[1].items.map((i) => i.c.id), ['c-19f4tb']);
  // The Mine filter keeps them; only merges, no open changes: the group alone.
  assert.deepEqual(railGroups(list, 'mine', 'updated', '', [merged]).map((x) => x.key), ['mine', 'merged']);
  assert.deepEqual(railGroups([], 'all', 'updated', '', [merged]).map((x) => x.key), ['merged']);
  assert.deepEqual(railHints(merged).map((h) => h.text), ['dennis']);
  // Filtered by the search like the open ones; other filters leave it out.
  assert.equal(railGroups(list, 'all', 'updated', 'podinfo', [merged]).some((x) => x.items.some((i) => i.c.id === 'c-19f4tb')), false);
  assert.equal(railGroups(list, 'review', 'updated', '', [merged])[0].items.some((i) => i.c.id === 'c-19f4tb'), false);
});
