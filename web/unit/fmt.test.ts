import { test } from 'node:test';
import assert from 'node:assert/strict';
import { shortDate, endsAt, ago, short, isChangeId, duration, clock } from '../src/lib/f/fmt.ts';

const now = new Date(2026, 9, 28, 9, 0, 0);

test('short date', () => {
  assert.equal(shortDate(new Date(2026, 9, 14, 10, 30), now), '14 Oct');
  assert.equal(shortDate(new Date(2025, 8, 3), now), '3 Sep 2025');
  assert.equal(shortDate('not a date', now), '');
  assert.equal(shortDate(undefined, now), '');
});

test('grant end', () => {
  assert.equal(endsAt(new Date(2026, 9, 28, 17, 40), now), 'ends 17:40');
  assert.equal(endsAt(new Date(2026, 9, 29, 1, 5), now), 'ends tomorrow 01:05');
  assert.equal(endsAt(new Date(2026, 9, 28, 8, 0), now), 'ended');
  assert.equal(clock(new Date(2026, 9, 28, 7, 4)), '07:04');
});

test('ago', () => {
  assert.equal(ago(new Date(now.getTime() - 10_000), now), 'just now');
  assert.equal(ago(new Date(now.getTime() - 5 * 60_000), now), '5 min ago');
  assert.equal(ago(new Date(now.getTime() - 3 * 3600_000), now), '3 h ago');
  assert.equal(ago(new Date(now.getTime() - 3 * 86400_000), now), '3 d ago');
});

test('ids and durations', () => {
  assert.equal(short('5749a4974305012471330d39ecb142e1f0cce87b'), '5749a49');
  assert.ok(isChangeId('c-2hd7vw'));
  assert.ok(!isChangeId('f186de7'));
  assert.equal(duration(480), '480 ms');
  assert.equal(duration(1234), '1.2 s');
  assert.equal(duration(400_000), '6m 40s');
});
