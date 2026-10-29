import { test } from 'node:test';
import assert from 'node:assert/strict';
import { entryNeedsScroll, promptScrollTop } from '../src/routes/llm-log/markscroll.ts';

test('prompt scroll: no marks leaves the box at the top', () => {
  assert.equal(promptScrollTop([], 400, 2000), 0);
});

test('prompt scroll: marks that fit are centred together', () => {
  // token at 1500, sops at 1525 (each 20 high): span 1500..1545, centre 1522.5
  const top = promptScrollTop(
    [
      { top: 1500, height: 20 },
      { top: 1525, height: 20 },
    ],
    400,
    2000,
  );
  assert.equal(top, 1323);
  assert.ok(top <= 1500 && top + 400 >= 1545, 'both marks are in the box');
});

test('prompt scroll: marks far apart put the first a third down', () => {
  const top = promptScrollTop(
    [
      { top: 600, height: 20 },
      { top: 1800, height: 20 },
    ],
    300,
    3000,
  );
  assert.equal(top, 500);
});

test('prompt scroll: clamped to the content', () => {
  assert.equal(promptScrollTop([{ top: 40, height: 20 }], 400, 2000), 0);
  assert.equal(promptScrollTop([{ top: 1980, height: 20 }], 400, 2000), 1600);
  assert.equal(promptScrollTop([{ top: 100, height: 20 }], 400, 300), 0);
});

test('entry scroll: only when the entry does not fit where it is', () => {
  const view = { top: 60, bottom: 864 };
  assert.equal(entryNeedsScroll({ top: 200, bottom: 800 }, view), false);
  assert.equal(entryNeedsScroll({ top: 500, bottom: 1200 }, view), true);
  assert.equal(entryNeedsScroll({ top: 20, bottom: 700 }, view), true);
});
