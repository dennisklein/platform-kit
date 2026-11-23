import { test } from 'node:test';
import assert from 'node:assert/strict';
import { forgePill } from '../src/lib/f/forge.ts';

test('forge pill', () => {
  assert.deepEqual(forgePill(undefined), { label: 'Forge', tone: '', title: '' });
  const ok = forgePill({ name: 'Gitea', reachable: true, url: 'http://gitea:3000' });
  assert.equal(ok.label, 'Gitea');
  assert.equal(ok.tone, 'ok');
  const down = forgePill({ name: 'Gitea', reachable: false, detail: 'connection refused' });
  assert.equal(down.label, 'Gitea offline');
  assert.equal(down.tone, 'warn');
  assert.ok(down.title.startsWith('Gitea offline: showing local state (connection refused).'));
  // The ping answers again, but the last fetch or poll failed: still offline.
  assert.equal(forgePill({ name: 'Gitea', reachable: true }, true).label, 'Gitea offline');
  assert.equal(forgePill(undefined, true).label, 'Gitea offline');
});
