// Change actions with feedback. Buttons read actions.busy to show progress.
import { api, must, ApiError } from '../api/client';
import type { Change, ChangeSummary, LabRun } from '../api/types';
import { toasts } from '../stores/toasts.svelte';
import { change } from '../stores/change.svelte';
import { changes } from '../stores/changes.svelte';
import { router } from '../router.svelte';
import { runLabel, shortSha } from './format';

export type ActionKey =
  | 'push'
  | 'ready'
  | 'rebase'
  | 'merge'
  | 'close'
  | 'lab'
  | 'checkout'
  | 'approve'
  | 'create'
  | 'revert';

class Actions {
  /** changeId → running action. */
  busy = $state<Record<string, ActionKey | undefined>>({});

  isBusy(id: string, key?: ActionKey): boolean {
    const b = this.busy[id];
    return key ? b === key : b !== undefined;
  }

  async #run<T>(id: string, key: ActionKey, fn: () => Promise<T>, fail: string): Promise<T | undefined> {
    if (this.busy[id]) return undefined;
    this.busy = { ...this.busy, [id]: key };
    try {
      const r = await fn();
      return r;
    } catch (e) {
      toasts.error(fail, e);
      return undefined;
    } finally {
      const { [id]: _done, ...rest } = this.busy;
      this.busy = rest;
      if (change.id === id) change.later(key);
      changes.later();
    }
  }

  push(id: string) {
    return this.#run(id, 'push', async () => {
      const r = await must(api.POST('/changes/{id}/push', { params: { path: { id } } }));
      const n = r.pushed?.length ?? 0;
      const what = n ? `Pushed ${n} commit${n === 1 ? '' : 's'}` : 'Pushed';
      if (r.renamedFrom) toasts.info(`The id ${r.renamedFrom} was taken on the forge; the change is now ${r.change.id}.`);
      if (r.mr) {
        toasts.ok(
          r.created ? `${what}. Draft MR !${r.mr.number} opened.` : `${what} to MR !${r.mr.number}.`,
          undefined,
          { label: `MR !${r.mr.number}`, href: r.mr.url },
        );
      } else toasts.ok(`${what}.`);
      if (r.renamedFrom && router.params.id === r.renamedFrom) {
        router.navigate(router.current.replace(r.renamedFrom, r.change.id));
      }
      return r;
    }, 'Push failed');
  }

  ready(id: string) {
    return this.#run(id, 'ready', async () => {
      const c = await must(api.POST('/changes/{id}/ready', { params: { path: { id } } }));
      toasts.ok(`Ready for review${c.mr ? `: MR !${c.mr.number}` : ''}.`, 'The metadata file moved to the MR description.');
      return c;
    }, 'Mark ready failed');
  }

  rebase(id: string) {
    return this.#run(id, 'rebase', async () => {
      try {
        const r = await must(api.POST('/changes/{id}/rebase', { params: { path: { id } } }));
        if (!r.ok) {
          toasts.push({ tone: 'bad', text: 'Rebase stopped on a conflict', detail: (r.conflicts ?? []).join(', ') });
          return r;
        }
        const parts = [`Rebased on ${r.change.target} ${shortSha(r.onto)}.`];
        if (r.impactUnchanged === true) parts.push('Impact unchanged.');
        else if (r.impactUnchanged === false) parts.push('The impact changed: review it again.');
        toasts.ok(parts.join(' '), 'The old lab run tested another sha: run the lab again.');
        return r;
      } catch (e) {
        if (e instanceof ApiError && e.status === 409 && e.paths?.length) {
          toasts.push({ tone: 'bad', text: 'Rebase conflict, nothing changed', detail: e.paths.join(', ') });
          return undefined;
        }
        throw e;
      }
    }, 'Rebase failed');
  }

  merge(id: string) {
    return this.#run(id, 'merge', async () => {
      const r = await must(api.POST('/changes/{id}/merge', { params: { path: { id } } }));
      toasts.ok(`Merged as ${shortSha(r.sha)}.`, (r.openSteps ?? []).map((s) => s.text).join(' ') || 'Flux applies it next.');
      return r;
    }, 'Merge refused');
  }

  close(id: string) {
    return this.#run(id, 'close', async () => {
      const r = await must(api.POST('/changes/{id}/close', { params: { path: { id } } }));
      toasts.ok(`Closed ${id}.`, r.mr ? `MR !${r.mr.number} is closed.` : 'Its worktree and branch are gone.');
      // Without an MR nothing records the change any more: leave its page.
      if (!r.mr && router.params.id === id) router.navigate('/changes');
      return r;
    }, 'Close failed');
  }

  approve(id: string, comment?: string) {
    return this.#run(id, 'approve', async () => {
      const r = await must(api.POST('/changes/{id}/approve', { params: { path: { id } }, body: { comment } }));
      toasts.ok(r.counts ? `Approved ${id}.` : `Approved ${id}, but it does not count: ${r.reason ?? ''}`);
      return r;
    }, 'Approve failed');
  }

  demoApprove(id: string, as: string) {
    return this.#run(id, 'approve', async () => {
      const r = await must(api.POST('/demo/approve', { body: { changeId: id, as } }));
      toasts.ok(r.counts ? `Approved as ${as}.` : `${as} approved, but it does not count: ${r.reason ?? ''}`);
      return r;
    }, `Approve as ${as} failed`);
  }

  checkout(id: string) {
    return this.#run(id, 'checkout', async () => {
      const r = await must(api.POST('/changes/{id}/checkout', { params: { path: { id } } }));
      toasts.ok(`Checked out ${id} into a local worktree.`);
      return r;
    }, 'Checkout failed');
  }

  /** Start a lab run and open the lab view. */
  runLab(id: string, scenario?: string) {
    return this.#run(id, 'lab', async () => {
      const r: LabRun = await must(
        api.POST('/changes/{id}/labs', { params: { path: { id } }, body: scenario ? { scenario } : {} }),
      );
      toasts.info(`Lab ${runLabel(r.id)} started.`);
      router.navigate(`/changes/${encodeURIComponent(id)}/lab?run=${encodeURIComponent(r.id)}`);
      return r;
    }, 'Lab did not start');
  }

  revert(id: string) {
    return this.#run(id, 'revert', async () => {
      const c: Change = await must(api.POST('/changes/{id}/revert', { params: { path: { id } } }));
      toasts.ok(`Revert change ${c.id} opened.`);
      router.navigate(`/changes/${encodeURIComponent(c.id)}`);
      return c;
    }, 'Revert failed');
  }
}

export const actions = new Actions();

/** The next step a person would take on a change, for the primary button. */
export type NextStep = 'commit' | 'push' | 'rebase' | 'lab' | 'ready' | 'approve' | 'merge' | 'checkout' | null;

export function nextStep(c: Change | ChangeSummary): NextStep {
  const g = 'gates' in c ? c.gates : undefined;
  const open = !['merged', 'deployed', 'verified', 'closed'].includes(c.state);
  if (!open) return null;
  if (c.group === 'review' && c.state === 'review') return 'approve';
  if (!c.local && c.group !== 'mine') return 'checkout';
  if (c.dirty) return 'commit';
  if (c.behindTarget > 0) return 'rebase';
  if (c.labState === 'running') return null;
  const labNeeded = g ? g.labGate.required && g.labGate.status !== 'met' : c.labState !== 'pass' && c.risk === 'high';
  if (labNeeded && c.labState !== 'pass') return 'lab';
  if (c.local && (!c.pushed || c.aheadRemote > 0)) return 'push';
  if (c.mr?.draft || (c.state === 'draft' && c.pushed)) return 'ready';
  if (g?.mergeable) return 'merge';
  return null;
}
