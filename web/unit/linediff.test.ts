import { test } from 'node:test';
import assert from 'node:assert/strict';
import { diffLines, hunks, stat, parseUnified } from '../src/lib/f/linediff.ts';

const ops = (a: string, b: string) => diffLines(a, b).map((l) => l.op + l.text).join('|');

test('equal texts have no changes', () => {
  const d = diffLines('a\nb\n', 'a\nb\n');
  assert.deepEqual(stat(d), { added: 0, removed: 0 });
  assert.equal(hunks(d).length, 0);
});

test('one changed line', () => {
  assert.equal(ops('a\nb\nc\n', 'a\nB\nc\n'), ' a|-b|+B| c');
});

test('insert and delete keep line numbers', () => {
  const d = diffLines('x\ny\nz', 'x\nnew\ny');
  assert.equal(d.map((l) => l.op + l.text).join('|'), ' x|+new| y|-z');
  assert.deepEqual(d.find((l) => l.op === '+'), { op: '+', text: 'new', b: 2 });
  assert.deepEqual(d.find((l) => l.op === '-'), { op: '-', text: 'z', a: 3 });
});

test('from empty', () => {
  assert.equal(ops('', 'a\nb'), '+a|+b');
  assert.equal(ops('a', ''), '-a');
});

test('hunks with context', () => {
  const a = Array.from({ length: 20 }, (_, i) => `l${i + 1}`).join('\n');
  const b = a.replace('l5\n', 'L5\n').replace('l18\n', 'L18\n');
  const h = hunks(diffLines(a, b), 2);
  assert.equal(h.length, 2);
  assert.equal(h[0].header, '@@ -3,5 +3,5 @@');
  assert.equal(h[1].lines.filter((l) => l.op === '+')[0].text, 'L18');
});

test('the cn-003 port move', () => {
  const old = 'ports:\n  - {name: p2, peer: {switch: data-b, port: 37}}\n';
  const now = 'ports:\n  - {name: p2, peer: {switch: data-a, port: 37}}\n';
  assert.deepEqual(stat(diffLines(old, now)), { added: 1, removed: 1 });
});

test('parse a unified diff', () => {
  const u = parseUnified('--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n');
  assert.deepEqual(u.map((l) => l.op), ['h', 'h', '@', ' ', '-', '+']);
  assert.equal(u[5].text, 'c');
});
