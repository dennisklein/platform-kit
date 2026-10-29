import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { buildForm, nextPortName, problemsAt } from '../../src/lib/change/formModel.ts';
import { at, checkValue, skeleton } from '../../src/lib/change/schema.ts';

const node = JSON.parse(readFileSync(new URL('./fixtures/schema-Node.json', import.meta.url), 'utf8'));
const helm = JSON.parse(readFileSync(new URL('./fixtures/schema-HelmRelease.json', import.meta.url), 'utf8'));

test('node form: sections from x-pk-ui groups, widgets', () => {
  const data = { apiVersion: 'inventory.pk.io/v1beta1', kind: 'Node', metadata: { name: 'cn-035' }, spec: { cluster: '', role: 'worker', location: { rack: '', unit: 1, size: 2 }, bmc: { mac: '' }, ports: [] } };
  const f = buildForm(node, data);
  const titles = f.map((s) => s.title);
  assert.ok(titles.includes('Intent'));
  assert.ok(titles.includes('Ports and cables'));
  const intent = f.find((s) => s.title === 'Intent')!;
  const kinds = Object.fromEntries(intent.rows.map((r) => [r.key, r.kind]));
  assert.equal(kinds.cluster, 'select');
  assert.equal(kinds.role, 'select');
  assert.equal(kinds.location, 'rack');
  const ports = f.find((s) => s.title === 'Ports and cables')!.rows[0];
  assert.equal(ports.kind, 'list');
  assert.deepEqual(ports.path, ['spec', 'ports']);
  const port = at(node, ['spec', 'ports', 0, 'peer']);
  assert.equal(port?.['x-pk-ui'].widget, 'port-picker');
});

test('helm form: duration, secret, metadata', () => {
  const data = { metadata: { name: 'cilium', namespace: 'kube-system' }, spec: { interval: '5m', chart: { spec: { chart: 'cilium', version: '1.18.2' } }, values: { bgpControlPlane: { enabled: true }, hubble: { ui: { auth: { token: '••••••••' } } } } } };
  const rows = buildForm(helm, data).flatMap((s) => s.rows);
  const by = Object.fromEntries(rows.map((r) => [r.path.join('.'), r]));
  assert.equal(by['spec.interval'].kind, 'duration');
  assert.equal(by['metadata.name'].kind, 'text');
  assert.equal(by['spec.values.hubble.ui.auth.token'].kind, 'secret');
  assert.equal(by['spec.values.hubble.ui.auth.token'].readOnly, true);
  assert.equal(checkValue(by['spec.interval'].schema, '5 minutes'), 'must be a duration, for example 5m');
  assert.equal(checkValue(by['spec.interval'].schema, '5m'), null);
});

test('skeleton, problems, port names', () => {
  const item = skeleton(node, at(node, ['spec', 'ports', 0])) as any;
  assert.ok('peer' in item && 'name' in item);
  const ps = [{ field: 'spec.ports[1].peer.port' }, { field: 'spec.cluster' }];
  assert.equal(problemsAt(ps, 'spec.ports[1].peer').length, 1);
  assert.equal(problemsAt(ps, 'spec.ports').length, 1);
  assert.equal(nextPortName([]), 'bmc');
  assert.equal(nextPortName([{ name: 'bmc' }, { name: 'p1' }]), 'p2');
});
