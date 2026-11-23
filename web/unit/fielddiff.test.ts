import { test } from 'node:test';
import assert from 'node:assert/strict';
import { flatten, compareFields } from '../src/lib/f/fielddiff.ts';

test('flatten maps and lists', () => {
  const f = flatten({ spec: { ports: [{ name: 'p1', peer: { port: 37 } }], tags: [], x: null } });
  assert.deepEqual([...f], [
    ['spec.ports[0].name', 'p1'],
    ['spec.ports[0].peer.port', '37'],
    ['spec.tags', '[]'],
    ['spec.x', 'null'],
  ]);
});

test('cn-003 since c-2hd7vw: a label added, key order ignored', () => {
  const then = { kind: 'Node', metadata: { name: 'cn-003' }, spec: { ports: [{ peer: { switch: 'data-a', port: 37 } }] } };
  const now = { metadata: { labels: { firmware: '2.4' }, name: 'cn-003' }, kind: 'Node', spec: { ports: [{ peer: { port: 37, switch: 'data-a' } }] } };
  assert.deepEqual(compareFields(then, now), [{ field: 'metadata.labels.firmware', op: 'add', after: '2.4' }]);
});

test('changed and deleted fields', () => {
  const r = compareFields({ a: 1, b: { c: 'x' } }, { a: 2 });
  assert.deepEqual(r, [
    { field: 'a', op: 'change', before: '1', after: '2' },
    { field: 'b.c', op: 'delete', before: 'x' },
  ]);
  assert.deepEqual(compareFields({ a: 1 }, { a: 1 }), []);
});
