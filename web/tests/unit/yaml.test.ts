import { test } from 'node:test';
import assert from 'node:assert/strict';
import { appendIn, deleteIn, formatPath, parsePath, readData, setIn } from '../../src/lib/change/yamlEdit.ts';
import { contextAt, pathAtLine } from '../../src/lib/change/yamlPath.ts';

const helm = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cilium # the CNI
  namespace: kube-system
spec:
  interval: 5m
  chart:
    spec:
      chart: cilium
      version: "1.18.2"
      sourceRef: { kind: HelmRepository, name: cilium }
  values:
    hubble:
      ui:
        auth:
          token: ••••••••
`;

test('paths parse and format', () => {
  assert.deepEqual(parsePath('spec.ports[1].peer.port'), ['spec', 'ports', 1, 'peer', 'port']);
  assert.equal(formatPath(['spec', 'ports', 1, 'peer', 'port']), 'spec.ports[1].peer.port');
  assert.deepEqual(parsePath('a[0][2].b'), ['a', 0, 2, 'b']);
});

test('scalar edits splice the text and keep comments and quotes', () => {
  const out = setIn(helm, ['spec', 'interval'], '10m');
  assert.equal(out, helm.replace('interval: 5m', 'interval: 10m'));
  const v = setIn(helm, ['spec', 'chart', 'spec', 'version'], '1.18.3');
  assert.ok(v.includes('version: "1.18.3"'));
  const n = setIn(helm, ['metadata', 'name'], 'cilium2');
  assert.ok(n.includes('name: cilium2 # the CNI'));
  const f = setIn(helm, ['spec', 'chart', 'spec', 'sourceRef', 'name'], 'cilium-oci');
  assert.ok(f.includes('sourceRef: { kind: HelmRepository, name: cilium-oci }'));
  // strings that need quotes get them
  const q = setIn(helm, ['spec', 'interval'], '5 minutes: x');
  assert.equal((readData(q).data as any).spec.interval, '5 minutes: x');
});

test('numbers keep their type', () => {
  const node = 'spec:\n  location:\n    rack: GC5A-6Z\n    unit: 1\n';
  const out = setIn(node, ['spec', 'location', 'unit'], 6);
  assert.equal(out, 'spec:\n  location:\n    rack: GC5A-6Z\n    unit: 6\n');
  assert.equal(setIn(node, ['spec', 'location', 'rack'], ''), 'spec:\n  location:\n    rack: ""\n    unit: 1\n');
});

test('structural edits add list items in flow style', () => {
  const node = 'spec:\n  cluster: ""\n  ports: []\n';
  let out = appendIn(node, ['spec', 'ports'], { name: 'bmc', cable: 'MGT-0141', peer: { switch: 'mgmt', port: 37 }, network: 'mgmt' });
  assert.equal(out, 'spec:\n  cluster: ""\n  ports:\n    - {name: bmc, cable: MGT-0141, peer: {switch: mgmt, port: 37}, network: mgmt}\n');
  out = appendIn(out, ['spec', 'ports'], { name: 'p1' });
  assert.ok(out.endsWith('    - {name: p1}\n'));
  out = setIn(out, ['spec', 'ports', 0, 'peer', 'port'], 38);
  assert.ok(out.includes('peer: {switch: mgmt, port: 38}'));
  out = deleteIn(out, ['spec', 'ports', 0]);
  assert.equal((readData(out).data as any).spec.ports.length, 1);
  // new map keys
  out = setIn(out, ['metadata', 'labels', 'firmware'], '2.4');
  assert.equal((readData(out).data as any).metadata.labels.firmware, '2.4');
});

test('missing list is created', () => {
  const out = appendIn('spec: {}\n', ['spec', 'ports'], { name: 'bmc' });
  assert.deepEqual((readData(out).data as any).spec.ports, [{ name: 'bmc' }]);
});

test('final newline is kept as in the file', () => {
  const noNL = 'a: 1\nb: [1]';
  assert.equal(appendIn(noNL, ['b'], 2).endsWith('\n'), false);
});

test('readData reports errors with a line', () => {
  const r = readData('a: 1\n  b: : x\n');
  assert.ok(r.error);
});

const node = `apiVersion: inventory.pk.io/v1beta1
kind: Node
metadata:
  name: cn-035
spec:
  cluster: defiant
  location:
    rack: GC5A-6Z
    unit: 6
  ports:
    - name: bmc
      peer:
        switch: mgmt
        port: 37
    - name: p1
      peer:
        switch: data-b
`;

test('context: key completion in a nested map', () => {
  const text = node + '        po';
  const c = contextAt(text, text.length);
  assert.deepEqual(c.parent, ['spec', 'ports', 1, 'peer']);
  assert.equal(c.inValue, false);
  assert.equal(c.prefix, 'po');
});

test('context: value completion', () => {
  const text = 'spec:\n  role: wor';
  const c = contextAt(text, text.length);
  assert.deepEqual(c.parent, ['spec']);
  assert.equal(c.key, 'role');
  assert.equal(c.inValue, true);
  assert.equal(c.prefix, 'wor');
  assert.equal(text.slice(c.from), 'wor');
});

test('context: new list item', () => {
  const text = node + '    - na';
  const c = contextAt(text, text.length);
  assert.deepEqual(c.parent, ['spec', 'ports', 2]);
  assert.equal(c.prefix, 'na');
});

test('context: top level and after a dedent', () => {
  assert.deepEqual(contextAt('ki', 2).parent, []);
  const text = node + 'st';
  assert.deepEqual(contextAt(text, text.length).parent, []);
  const t2 = node + '  clu';
  assert.deepEqual(contextAt(t2, t2.length).parent, ['spec']);
});

test('pathAtLine for hover', () => {
  assert.deepEqual(pathAtLine(node, 9), ['spec', 'location', 'unit']);
  assert.deepEqual(pathAtLine(node, 14), ['spec', 'ports', 0, 'peer', 'port']);
  assert.deepEqual(pathAtLine(node, 15), ['spec', 'ports', 1, 'name']);
  assert.equal(pathAtLine(node, 1)?.join('.'), 'apiVersion');
});

test('an empty quoted placeholder takes the plain style', () => {
  assert.equal(setIn('spec:\n  cluster: ""\n', ['spec', 'cluster'], 'defiant'), 'spec:\n  cluster: defiant\n');
  const flow = 'ports:\n  - {name: p1, cable: "", peer: {switch: "", port: 37}}\n';
  let out = setIn(flow, ['ports', 0, 'peer', 'switch'], 'data-b');
  out = setIn(out, ['ports', 0, 'cable'], 'DATA-0201');
  assert.equal(out, 'ports:\n  - {name: p1, cable: DATA-0201, peer: {switch: data-b, port: 37}}\n');
  // quotes stay where the old value had them, and where YAML needs them
  assert.equal(setIn('a: "x"\n', ['a'], 'y'), 'a: "y"\n');
  assert.equal(setIn('a: ""\n', ['a'], '2.4'), 'a: "2.4"\n');
});

test('new keys go to their schema place, not to the end', () => {
  const order = (p: (string | number)[]) =>
    p.join('.') === 'spec' ? ['cluster', 'role', 'location', 'model', 'serial', 'bmc', 'ports'] : p.length === 0 ? ['apiVersion', 'kind', 'metadata', 'spec'] : undefined;
  const node = 'kind: Node\nspec:\n  cluster: defiant\n  role: worker\n  bmc:\n    mac: ""\n  ports: []\n';
  let out = setIn(node, ['spec', 'model'], 'Supermicro SYS-220U-TNR', order);
  assert.equal(out, 'kind: Node\nspec:\n  cluster: defiant\n  role: worker\n  model: Supermicro SYS-220U-TNR\n  bmc:\n    mac: ""\n  ports: []\n');
  // a new nested object: its first new level is placed
  out = setIn(out, ['spec', 'location', 'rack'], 'GC5A-6Z', order);
  assert.ok(out.includes('  role: worker\n  location:\n    rack: GC5A-6Z\n  model:'));
  // without an order the key goes to the end, as before
  assert.ok(setIn(node, ['spec', 'serial'], 'X').endsWith('  serial: X\n'));
});

test('typing through a value that needs quotes leaves a plain scalar plain', () => {
  // The form writes each keystroke: "5" needs quotes on its way to "5m".
  let t = helm;
  for (const v of ['', '5', '5m']) t = setIn(t, ['spec', 'interval'], v, undefined, helm);
  assert.equal(t, helm);
  // "5 minutes" was saved by the YAML editor and is the loaded text now.
  const loaded = helm.replace('interval: 5m', 'interval: 5 minutes');
  t = loaded;
  for (const v of ['', '5', '5m']) t = setIn(t, ['spec', 'interval'], v, undefined, loaded);
  assert.equal(t, helm);
  // A value quoted on disk keeps its quotes.
  t = helm;
  for (const v of ['1', '1.', '1.18', '1.18.3']) t = setIn(t, ['spec', 'chart', 'spec', 'version'], v, undefined, helm);
  assert.ok(t.includes('version: "1.18.3"'));
  // A trailing space on the way to the final value does not stick.
  const node = 'spec:\n  model: ""\n  bmc: { mac: "" }\n';
  t = node;
  for (const v of ['Supermicro', 'Supermicro ', 'Supermicro SYS']) t = setIn(t, ['spec', 'model'], v, undefined, node);
  for (const v of ['3', '3c', '3c:ec:ef:a1:0c:35']) t = setIn(t, ['spec', 'bmc', 'mac'], v, undefined, node);
  assert.equal(t, 'spec:\n  model: Supermicro SYS\n  bmc: { mac: 3c:ec:ef:a1:0c:35 }\n');
  // Without an origin the style of the current node is kept (old behaviour).
  assert.equal(setIn('a: "5"\n', ['a'], '5m'), 'a: "5m"\n');
});
