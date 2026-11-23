import { test } from 'node:test';
import assert from 'node:assert/strict';
import { demoTarget } from '../src/lib/f/demo.ts';

const list = [
  { id: 'c-31d0aa', state: 'approved' },
  { id: 'c-6m1pqd', state: 'review' },
  { id: 'c-k7m2qz', state: 'review' },
];

test('demo target: explicit pick wins', () => {
  assert.equal(demoTarget(list, { picked: 'c-31d0aa', onScreen: 'c-k7m2qz' }), 'c-31d0aa');
});

test('demo target: change on screen, then the working change', () => {
  assert.equal(demoTarget(list, { onScreen: 'c-k7m2qz', working: 'c-6m1pqd' }), 'c-k7m2qz');
  assert.equal(demoTarget(list, { onScreen: 'c-zzzzzz', working: 'c-k7m2qz' }), 'c-k7m2qz');
});

test('demo target: a change in review, else the first, else none', () => {
  assert.equal(demoTarget(list), 'c-6m1pqd');
  assert.equal(demoTarget([{ id: 'c-31d0aa', state: 'approved' }]), 'c-31d0aa');
  assert.equal(demoTarget([]), '');
});

test('demo target: a working change that moved on to approved stays the target', () => {
  // After the first approval the change may be "approved"; it must not jump.
  const after = [
    { id: 'c-6m1pqd', state: 'review' },
    { id: 'c-k7m2qz', state: 'approved' },
  ];
  assert.equal(demoTarget(after, { working: 'c-k7m2qz' }), 'c-k7m2qz');
});
