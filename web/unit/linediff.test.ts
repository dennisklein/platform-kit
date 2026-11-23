import { test } from 'node:test';
import assert from 'node:assert/strict';
import { diffLines, hunks, stat, parseUnified, wordSegs, wordHighlights } from '../src/lib/f/linediff.ts';

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

const marked = (segs: { text: string; changed: boolean }[]) => segs.map((s) => (s.changed ? `[${s.text}]` : s.text)).join('');

test('word segments: whole words, not single digits', () => {
  const w = wordSegs('  version: 6.8.0', '  version: 6.9.0')!;
  assert.equal(marked(w.a), '  version: 6.[8].0');
  assert.equal(marked(w.b), '  version: 6.[9].0');
  const p = wordSegs('a: {switch: data-a, port: 47}', 'a: {switch: data-a, port: 48}')!;
  assert.equal(marked(p.a), 'a: {switch: data-a, port: [47]}');
  assert.equal(marked(p.b), 'a: {switch: data-a, port: [48]}');
});

test('word segments: insertion and unrelated lines', () => {
  const w = wordSegs('speed: 10G', 'speed: 100G')!;
  assert.equal(marked(w.a), 'speed: [10G]');
  assert.equal(marked(w.b), 'speed: [100G]');
  assert.equal(wordSegs('kind: Node', 'metadata:'), null);
  assert.equal(wordSegs('same', 'same'), null);
});

test('word highlights pair removed and added runs', () => {
  const lines = parseUnified('@@ -1,3 +1,3 @@\n a: 1\n-b: old-value\n-c: 2\n+b: new-value\n+c: 3\n+d: 4\n');
  const h = wordHighlights(lines);
  assert.deepEqual([...h.keys()].sort(), [2, 3, 4, 5]);
  assert.equal(marked(h.get(4)!), 'b: [new]-value');
  assert.equal(h.has(6), false);
});
