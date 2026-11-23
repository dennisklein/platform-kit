// Shared mock fixtures: identity, bench status, focus and the change list of
// the seed story. Handlers of E and F can read and change `seed`.
import { on } from './registry';
import me from './fixtures/me.json';
import status from './fixtures/status.json';
import focus from './fixtures/focus.json';
import changes from './fixtures/changes.json';

export const seed = {
  me: me as any,
  status: status as any,
  focus: { ...(focus as any), changeId: 'c-k7m2qz', route: '/changes/c-k7m2qz', view: 'change' } as any,
  changes: changes as any,
};

on('GET', '/me', () => seed.me);
on('GET', '/status', () => seed.status);
on('GET', '/focus', () => seed.focus);
on('PUT', '/focus', ({ body }) => {
  seed.focus = { ...body, pinned: body?.pinned ?? seed.focus.pinned, by: seed.me.id, at: new Date().toISOString() };
  return seed.focus;
});
on('GET', '/changes', ({ query }) => {
  const all = query.get('scope') === 'all';
  const list = seed.changes.changes.filter(
    (c: any) => all || !['merged', 'deployed', 'verified', 'closed'].includes(c.state),
  );
  return { ...seed.changes, changes: list };
});
