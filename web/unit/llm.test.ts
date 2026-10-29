import { test } from 'node:test';
import assert from 'node:assert/strict';
import { llmPill, modelLabel, needsStatusCheck, offlineReason } from '../src/lib/f/llm.ts';

const err = 'llm: Get "http://host.docker.internal:11434/v1/models": dial tcp 172.17.0.1:11434: connect: connection refused';

test('offline reason', () => {
  assert.equal(offlineReason(err), 'connection refused');
  assert.equal(offlineReason('llm: something odd: model not loaded'), 'model not loaded');
  assert.equal(offlineReason(undefined), 'not reachable');
});

test('top bar pill', () => {
  const off = llmPill({ reachable: false, mock: false, model: 'qwen3:14b', baseUrl: 'http://host.docker.internal:11434/v1', detail: err });
  assert.equal(off.label, 'LLM offline');
  assert.equal(off.tone, 'warn');
  assert.ok(off.title.startsWith('qwen3:14b at http://host.docker.internal:11434/v1: connection refused'));
  assert.equal(llmPill({ reachable: true, mock: false, model: 'qwen3:14b' }).label, 'LLM qwen3:14b');
  assert.equal(llmPill({ reachable: true, mock: true, model: 'mock' }).label, 'LLM built-in');
});

test('answer model line', () => {
  assert.equal(modelLabel({ mock: true, model: 'mock' }).text, 'built-in · from project data');
  assert.equal(modelLabel({ mock: false, model: 'qwen3:14b' }).text, 'qwen3:14b');
  assert.equal(modelLabel({ mock: false, model: 'qwen3:14b' }, true).text, 'from project data');
  // The bench field wins over the status: a fallback answer while the status still says reachable.
  assert.equal(modelLabel({ mock: false, model: 'qwen3:14b', fallback: true }).text, 'from project data');
  assert.equal(modelLabel({ mock: false, model: 'qwen3:14b', fallback: false }, true).text, 'qwen3:14b');
  // Mock answers are data answers too, but say so as "built-in".
  assert.equal(modelLabel({ mock: true, model: 'mock', fallback: true }).text, 'built-in · from project data');
  assert.equal(modelLabel({ mock: false, model: 'qwen3:14b', fallback: true }).fallback, true);
  assert.equal(modelLabel({ mock: false, model: 'qwen3:14b', fallback: false }, true).fallback, false);
  assert.equal(modelLabel({ mock: false, model: 'fallback (qwen3:14b offline)' }).fallback, true);
});

test('fallback answers refresh the status', () => {
  assert.equal(needsStatusCheck({ mock: false, model: 'qwen3:14b', fallback: true }), true);
  assert.equal(needsStatusCheck({ mock: false, model: 'qwen3:14b', fallback: false }), false);
  assert.equal(needsStatusCheck({ mock: true, model: 'mock', fallback: true }), false);
  assert.equal(needsStatusCheck(null), false);
});
