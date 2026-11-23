// The change that the page shows (detail, files, review, lab), with live
// reloads from bench events. Pages call change.use(id).
import { api, must, errorText } from '../api/client';
import type { Change, ChangeEvent, ForgeEvent, LabRun, RenderEvent } from '../api/types';
import { events } from './events.svelte';

class ChangeStore {
  id = $state<string | null>(null);
  data = $state<Change | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);
  /** Bumps on every reload, so pages can refetch their own data (impact, tree). */
  version = $state(0);
  /** Why the last reload happened (event reason), for pages that care. */
  reason = $state('');

  #seq = 0;
  #timer: ReturnType<typeof setTimeout> | undefined;
  #started = false;

  /** Show this change. Loads it when the id differs. */
  use(id: string | undefined | null) {
    this.#start();
    const next = id ?? null;
    if (next === this.id) return;
    this.id = next;
    this.data = null;
    this.error = null;
    if (next) void this.load();
  }

  /** Reload soon (merges bursts of events). */
  later(reason = '', ms = 150) {
    this.reason = reason;
    clearTimeout(this.#timer);
    this.#timer = setTimeout(() => void this.load(), ms);
  }

  async load() {
    const id = this.id;
    if (!id) return;
    const seq = ++this.#seq;
    this.loading = true;
    try {
      const c = await must(api.GET('/changes/{id}', { params: { path: { id } } }));
      if (seq !== this.#seq || id !== this.id) return;
      this.data = c;
      this.error = null;
      this.version++;
    } catch (e) {
      if (seq !== this.#seq) return;
      this.error = errorText(e);
    } finally {
      if (seq === this.#seq) this.loading = false;
    }
  }

  #start() {
    if (this.#started) return;
    this.#started = true;
    const mine = (id?: string) => !!id && id === this.id;
    events.on<ChangeEvent>('change.updated', (d) => mine(d.changeId) && this.later(d.reason ?? 'updated'));
    events.on<ChangeEvent>('change.created', (d) => mine(d.changeId) && this.later('created'));
    events.on<ForgeEvent>('forge.updated', (d) => (!d.changeId || mine(d.changeId)) && this.later(`forge.${d.what}`, 300));
    events.on<LabRun>('lab.finished', (d) => mine(d.changeId) && this.later('lab'));
    events.on<RenderEvent>('render.updated', (d) => {
      if (mine(d.changeId) && this.data) this.data = { ...this.data, impact: d.summary };
    });
    events.on('flux.updated', () => {
      const s = this.data?.state;
      if (s === 'merged' || s === 'deployed') this.later('flux', 500);
    });
  }
}

export const change = new ChangeStore();
