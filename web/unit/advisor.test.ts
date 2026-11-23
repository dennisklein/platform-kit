import { test } from 'node:test';
import assert from 'node:assert/strict';
import { minWorkingMs, readingSteps, resultMarkdown, suggestedBy, typedText } from '../src/lib/f/advisor.ts';

test('advisor: reading steps follow the recipe tools', () => {
  assert.deepEqual(readingSteps(['impact', 'policy']), ['Reading the rendered impact', 'Reading .pk/policy.yaml', 'Writing the answer']);
  assert.deepEqual(readingSteps(['files'], 'Writing the message'), ['Reading the changed files', 'Writing the message']);
  assert.deepEqual(readingSteps(['odd', 'odd']), ['Reading odd', 'Writing the answer']);
  assert.deepEqual(readingSteps(undefined), ['Writing the answer']);
});

test('advisor: the working card stays long enough to read, unless reduced motion', () => {
  assert.equal(minWorkingMs(6), 1600);
  assert.equal(minWorkingMs(2), 480);
  assert.equal(minWorkingMs(6, true), 0);
});

test('advisor: answer as Markdown', () => {
  const md = resultMarkdown({
    title: 'Risks of this change',
    changeId: 'c-k7m2qz',
    note: 'Advisory only.',
    points: [{ title: 'BGP policy', text: 'cn-036 gets no BGP session.', severity: 'risk', sources: [{ label: 'bgp-rack-6z.yaml' }] }],
  });
  assert.equal(
    md,
    '### Risks of this change (c-k7m2qz)\n\n1. [risk] **BGP policy.** cn-036 gets no BGP session. (bgp-rack-6z.yaml)\n\n_Advisory only._',
  );
});

test('advisor: suggestion badge', () => {
  assert.equal(suggestedBy('chat'), 'Suggested by chat');
  assert.equal(suggestedBy('advisor:check-commit'), 'Suggested by advisor · check commit');
  assert.equal(suggestedBy('advisor'), 'Suggested by advisor');
  assert.equal(suggestedBy(undefined), 'Suggested edit');
});

test('advisor: typed text ends within the time cap', () => {
  const t = 'feat(inventory): add cn-035 and cn-036 in GC5A-6Z';
  assert.equal(typedText(t, 0), '');
  assert.ok(typedText(t, 100).length > 0 && typedText(t, 100).length < t.length);
  assert.equal(typedText(t, 1200), t);
  assert.equal(typedText('x'.repeat(2000), 1200).length, 2000);
});
