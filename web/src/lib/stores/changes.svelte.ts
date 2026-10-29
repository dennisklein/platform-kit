// Open changes for the change rail. Reloads on change and forge events.
import { api, must, errorText } from '../api/client';
import type { ChangeList, ChangeSummary } from '../api/types';
import { events } from './events.svelte';
import { toasts } from './toasts.svelte';

const DONE = new Set(['merged', 'deployed', 'verified', 'closed']);
/** Own merges stay in the rail this long, so their rollout can be watched. */
const RECENT_MS = 4 * 24 * 3600 * 1000;

class Changes {
  /** Open changes. */
  list = $state<ChangeSummary[]>([]);
  /** My changes merged lately (newest first, at most 3), for the rail. */
  recent = $state<ChangeSummary[]>([]);
  main = $state<ChangeList['main'] | undefined>(undefined);
  /** The last fetch or forge poll failed: the list is the local state. */
  forgeOffline = $state(false);
  loading = $state(false);
  error = $state<string | null>(null);
  #timer: ReturnType<typeof setTimeout> | undefined;

  /** Reload soon (merges bursts of events). */
  later(ms = 200) {
    clearTimeout(this.#timer);
    this.#timer = setTimeout(() => void this.load(), ms);
  }

  start() {
    void this.load();
    const later = () => this.later();
    events.on('change.created', later);
    events.on('change.updated', later);
    events.on('forge.updated', later);
    events.on('lab.finished', later);
    events.on('flux.updated', later);
    // After a bench restart or a forge reset the list may be stale.
    events.onReconnect(later);
    events.on('forge.reset', () => this.rebuilt());
    window.addEventListener('pk:forge-reset', () => this.rebuilt());
    // A slow refresh while the tab is visible: nothing stays stale for long
    // even when an event was missed.
    setInterval(() => {
      if (document.visibilityState === 'visible') void this.load();
    }, 30_000);
  }

  #rebuiltAt = 0;
  /** The forge state was rebuilt: reload and say so. */
  rebuilt() {
    void this.load();
    if (Date.now() - this.#rebuiltAt < 10_000) return;
    this.#rebuiltAt = Date.now();
    toasts.push({ tone: 'warn', text: 'The forge state was rebuilt (reset).', detail: 'The change list is reloaded. Reload the page if a view still shows old changes.' }, 12_000);
  }

  async load() {
    this.loading = true;
    try {
      const r = await must(api.GET('/changes', { params: { query: { scope: 'all' } } }));
      const all = r.changes ?? [];
      this.list = all.filter((c) => !DONE.has(c.state));
      const since = Date.now() - RECENT_MS;
      this.recent = all
        .filter((c) => c.group === 'mine' && c.state !== 'closed' && DONE.has(c.state) && c.mergedAt && Date.parse(c.mergedAt) >= since)
        .sort((a, b) => (b.mergedAt ?? '').localeCompare(a.mergedAt ?? ''))
        .slice(0, 3);
      this.main = r.main;
      this.forgeOffline = !!r.forgeOffline;
      this.error = null;
    } catch (e) {
      this.error = errorText(e);
    } finally {
      this.loading = false;
    }
  }

  byId(id: string | undefined): ChangeSummary | undefined {
    return id ? (this.list.find((c) => c.id === id) ?? this.recent.find((c) => c.id === id)) : undefined;
  }
}

export const changes = new Changes();
