import { test } from 'node:test';
import assert from 'node:assert/strict';
import { changeOfRun, consoleLines, elapsed, parseReport, reportStatus } from '../src/routes/lab/labview.ts';
import type { LabStep } from '../src/lib/api/types.ts';

const step = (index: number, name: string, status: LabStep['status'], extra: Partial<LabStep> = {}): LabStep => ({
  index,
  name,
  type: 'check',
  title: name,
  status,
  ...extra,
});

test('console: finished steps with output, running step last, pending steps left out', () => {
  const lines = consoleLines([
    step(2, 'join', 'running', { startedAt: '2026-10-29T08:00:00Z' }),
    step(0, 'topology', 'pass', { detail: 'generate', durationMs: 300, output: ['[00:01] a', '[00:02] b'] }),
    step(1, 'bmc', 'pass', { output: ['[00:09] c'] }),
    step(3, 'checks', 'pending'),
  ]);
  assert.deepEqual(
    lines.map((l) => `${l.kind}:${l.kind === 'out' ? l.text : l.step}`),
    ['head:topology', 'out:[00:01] a', 'out:[00:02] b', 'head:bmc', 'out:[00:09] c', 'running:join'],
  );
  const head = lines[0];
  assert.equal(head.kind === 'head' && head.title, 'topology · generate');
});

test('console: streamed lines are capped, the check summary waits for all lines', () => {
  const s = step(0, 'bgp-up', 'fail', {
    output: ['[02:19] cn-037: no BGP peering policy selects it', '[02:30] done'],
    checks: [
      { name: 'bgp: selected', ok: false, detail: 'cn-037: no BGP peering policy selects it' },
      { name: 'bgp: session', ok: true, detail: 'ok' },
    ],
  });
  const partial = consoleLines([s], { 'bgp-up': 1 });
  assert.equal(partial.length, 2);
  const out = partial[1];
  assert.ok(out.kind === 'out' && out.bad, 'a line of a failed check is marked');
  const all = consoleLines([s]);
  const last = all[all.length - 1];
  assert.deepEqual(last.kind === 'checks' && [last.pass, last.fail], [1, 1]);
});

test('elapsed clock', () => {
  assert.equal(elapsed(0), '0:00');
  assert.equal(elapsed(7_400), '0:07');
  assert.equal(elapsed(83_000), '1:23');
  assert.equal(elapsed(-5), '0:00');
  assert.equal(elapsed(Number.NaN), '0:00');
});

test('report: steps, checks and result of a seeded lab report', () => {
  const body = `# Lab report c-p4x8ma/run-1

Result: **fail**. Driver sim, host local, provisioning metal, fresh machines.

## Steps

| Step | Detail | Result |
| --- | --- | --- |
| topology | generate from inventory and cabling | pass |
| BGP | peers | fail |
| checks | skipped | skipped |

## Checks

- pass: bgp: session to 10.6.0.1 established
- FAIL: bgp: session to 10.6.0.3 not established

## Failures

- cn-036: no BGP peering policy selects it

The lab emulates only the router 10.6.0.1.`;
  const r = parseReport(body);
  assert.equal(r.result, 'fail');
  assert.deepEqual(
    r.steps.map((s) => [s.title, s.result]),
    [
      ['topology', 'pass'],
      ['BGP', 'fail'],
      ['checks', 'skipped'],
    ],
  );
  assert.deepEqual(r.checks, [
    { ok: true, text: 'bgp: session to 10.6.0.1 established' },
    { ok: false, text: 'bgp: session to 10.6.0.3 not established' },
  ]);
  assert.deepEqual(r.failures, ['cn-036: no BGP peering policy selects it']);
  assert.deepEqual(r.notes, ['The lab emulates only the router 10.6.0.1.']);
  assert.deepEqual(parseReport('no sections'), { result: undefined, steps: [], checks: [], failures: [], notes: [] });
});

test('report: bench reports have a lab time column', () => {
  const r = parseReport('## Steps\n\n| Step | Detail | Result | Lab time |\n| --- | --- | --- | --- |\n| check: nodes Ready | 2 of 2 | pass | 15s |\n');
  assert.deepEqual(r.steps, [{ title: 'check: nodes Ready', detail: '2 of 2', result: 'pass' }]);
});

test('report status and run ids', () => {
  assert.equal(reportStatus('pass'), 'done');
  assert.equal(reportStatus('fail'), 'fail');
  assert.equal(reportStatus('skipped'), 'skipped');
  assert.equal(reportStatus('?'), 'todo');
  assert.equal(changeOfRun('c-31d0aa/run-1'), 'c-31d0aa');
  assert.equal(changeOfRun('run-1'), undefined);
});
