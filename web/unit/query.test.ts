import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseQuery, formatQuery, fromParams, toParams } from '../src/lib/f/query.ts';

test('parse filters and free text', () => {
  assert.deepEqual(parseQuery('entity:cn-003 author:jonas BGP peers'), { entity: 'cn-003', author: 'jonas', q: 'BGP peers' });
  assert.deepEqual(parseQuery('  path:control/** kind:Node '), { path: 'control/**', kind: 'Node' });
  assert.deepEqual(parseQuery('Kind:Node'), { kind: 'Node' });
});

test('quoted values and phrases', () => {
  assert.deepEqual(parseQuery('path:"kb/my notes/**" "break glass"'), { path: 'kb/my notes/**', q: 'break glass' });
});

test('unknown keys and empty values stay text or drop', () => {
  assert.deepEqual(parseQuery('Approved-by: anna'), { q: 'Approved-by: anna' });
  assert.deepEqual(parseQuery('foo:bar entity:'), { q: 'foo:bar' });
  assert.deepEqual(parseQuery(''), {});
});

test('format round trip', () => {
  const q = { q: 'disk', author: 'anna', entity: 'sn-05', path: 'a b/**' };
  assert.equal(formatQuery(q), 'entity:sn-05 path:"a b/**" author:anna disk');
  assert.deepEqual(parseQuery(formatQuery(q)), q);
});

test('url params', () => {
  const p = new URLSearchParams('entity=cn-003&q=port&x=1');
  assert.deepEqual(fromParams(p), { entity: 'cn-003', q: 'port' });
  assert.deepEqual(toParams({ entity: 'cn-003', q: '' }), { entity: 'cn-003' });
});

test('filters inside ?q= (links built from search text)', () => {
  assert.deepEqual(fromParams(new URLSearchParams('q=path:clusters/defiant/**')), { path: 'clusters/defiant/**' });
  assert.deepEqual(fromParams(new URLSearchParams('q=entity:cn-003 port&entity=cn-036')), { entity: 'cn-036', q: 'port' });
  assert.deepEqual(fromParams(new URLSearchParams('q=c-2hd7vw')), { q: 'c-2hd7vw' });
});
