import { test } from 'node:test';
import assert from 'node:assert/strict';
import { redactedFields } from '../src/routes/llm-log/redaction.ts';

test('redacted fields: key paths from the context block', () => {
  const prompt = `[change c-pgv3zk]
user: What is the hubble UI token?
--- context: clusters/defiant/infra/cilium/helmrelease.yaml (2 values redacted) ---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  interval: 5m
  values:
    hubble:
      relay:
        enabled: true
      ui:
        enabled: true
        auth:
          token: [redacted]
sops: [redacted]`;
  assert.deepEqual(redactedFields(prompt), [
    { file: 'clusters/defiant/infra/cilium/helmrelease.yaml', field: 'hubble.ui.auth.token' },
    { file: 'clusters/defiant/infra/cilium/helmrelease.yaml', field: 'sops' },
  ]);
});

test('redacted fields: list items and values outside YAML', () => {
  const prompt = `--- context: a.yaml ---
items:
  - name: x
    password: [redacted]
the key is [redacted]`;
  assert.deepEqual(redactedFields(prompt), [
    { file: 'a.yaml', field: 'items.password' },
    { file: 'a.yaml', field: '(value)' },
  ]);
  assert.deepEqual(redactedFields('no secrets here'), []);
});
