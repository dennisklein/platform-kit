import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentName, callArgs, reasonText, routeLabel, toCall } from '../src/lib/f/mcp.ts';

const base = { actor: 'dennis-agent', kind: 'agent' as const, at: '2026-10-30T10:00:00Z' };

test('mcp call: ok', () => {
  const c = toCall({ ...base, tool: 'pk_file_write', text: 'pk_file_write control/inventory/cn-035.yaml' });
  assert.equal(c.failed, false);
  assert.equal(c.text, 'pk_file_write control/inventory/cn-035.yaml');
  assert.equal(c.reason, undefined);
});

test('mcp call: approve and merge are denied for agents', () => {
  const c = toCall({ ...base, tool: 'pk_change_approve', text: 'pk_change_approve c-k7m2qz (failed)' });
  assert.equal(c.failed, true);
  assert.equal(c.text, 'pk_change_approve c-k7m2qz');
  assert.equal(c.reason, 'denied: agents never approve or merge');
});

test('mcp call: other failures', () => {
  const c = toCall({ ...base, tool: 'pk_lab_run', text: 'pk_lab_run c-k7m2qz (failed)' });
  assert.equal(c.failed, true);
  assert.equal(c.reason, 'failed');
  assert.equal(toCall({ ...base, tool: 'pk_change_list' }).text, 'pk_change_list');
});

test('mcp call: the bench sends the reason', () => {
  const c = toCall({ ...base, tool: 'pk_change_approve', text: 'pk_change_approve c-k7m2qz (failed: denied: agents never approve or merge)' });
  assert.equal(c.failed, true);
  assert.equal(c.denied, true);
  assert.equal(c.text, 'pk_change_approve c-k7m2qz');
  assert.equal(c.reason, 'denied: agents never approve or merge');
  assert.equal(reasonText(c.reason), 'Agents never approve or merge');
  assert.equal(callArgs(c), 'c-k7m2qz');
  const v = toCall({ ...base, tool: 'pk_file_write', text: 'pk_file_write cn-037.yaml (failed: invalid: spec.unit: must be ≥ 1)' });
  assert.equal(v.denied, false);
  assert.equal(v.reason, 'invalid: spec.unit: must be ≥ 1');
  assert.equal(toCall({ ...base, tool: 'pk_lab_run', text: 'pk_lab_run c-k7m2qz (failed)' }).denied, false);
});

test('mcp: routes in words', () => {
  assert.equal(routeLabel('/changes/c-k7m2qz'), 'change c-k7m2qz');
  assert.equal(routeLabel('/changes/c-k7m2qz/review'), 'review of c-k7m2qz');
  assert.equal(routeLabel('/changes/c-k7m2qz/review?advisor=risks&by=dennis%2Fagent'), 'review of c-k7m2qz · advisor');
  assert.equal(routeLabel('/changes/c-k7m2qz/files?path=control%2Finventory%2Fcn-037.yaml'), 'cn-037.yaml in c-k7m2qz');
  assert.equal(routeLabel('/changes/c-k7m2qz/lab'), 'lab of c-k7m2qz');
  assert.equal(routeLabel('/assets/racks/GC5A-6Z'), 'rack GC5A-6Z');
  assert.equal(routeLabel('/assets/Node/cn-035'), 'Node cn-035');
  assert.equal(routeLabel('/kb/article?path=kb%2Frunbooks%2Fadd-node.md'), 'knowledge: add-node.md');
  assert.equal(routeLabel('/chat'), 'chat');
});

test('mcp call: agent name', () => {
  assert.equal(agentName({ actor: 'dennis-agent', actsFor: 'dennis' }), 'dennis/agent');
  assert.equal(agentName({ actor: 'sync-bot' }), 'sync-bot');
});
