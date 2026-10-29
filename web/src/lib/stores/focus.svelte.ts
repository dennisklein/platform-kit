// Server-side focus: the change, file and view this user works on. All
// tabs, the chat and agents follow it. Pages call focus.set() when the user
// opens something; focus.changed events update it from other tabs/agents.
import { untrack } from 'svelte';
import { api, must } from '../api/client';
import type { Focus, FocusUpdate, Presence } from '../api/types';
import { events } from './events.svelte';
import { router } from '../router.svelte';

class FocusStore {
  current = $state<Focus | null>(null);
  /** Last presence per actor (agents working in the bench). */
  presence = $state<Record<string, Presence>>({});
  /** A focus move that an agent suggests; the UI asks before it follows. */
  suggestion = $state<Presence | null>(null);
  /** Follow focus.changed from other tabs and agents by navigating. */
  follow = $state(true);
  /**
   * The last move an agent made, for the "Following dennis/agent" cue.
   * followed: the screen moved with it (false while following is paused).
   */
  lastMove = $state<{ by: string; route: string; at: number; followed: boolean } | null>(null);
  /** Agent banners in view: they show the follow state, so the floating cue steps back. */
  banners = $state(0);
  /** Agent banners in view per change: they show a denial, so no toast repeats it. */
  bannerChanges = $state<Record<string, number>>({});

  #pending: FocusUpdate | null = null;
  #timer: ReturnType<typeof setTimeout> | undefined;

  start() {
    void this.load();
    events.on<Focus>('focus.changed', (f) => this.#apply(f));
    events.on<Presence>('presence', (p, ev) => {
      const prev = this.presence[p.actor];
      // A recall after a reload never replaces newer presence.
      if (!ev.replay || !prev || prev.at <= p.at) this.presence = { ...this.presence, [p.actor]: p };
      if (p.suggest && !ev.replay) this.suggestion = p;
    });
  }

  /** An agent banner of a change came into view (+1) or left it (-1). */
  bannerSeen(changeId: string | undefined, d: 1 | -1) {
    this.banners = Math.max(0, this.banners + d);
    if (!changeId) return;
    const n = Math.max(0, (this.bannerChanges[changeId] ?? 0) + d);
    const next = { ...this.bannerChanges };
    if (n) next[changeId] = n;
    else delete next[changeId];
    this.bannerChanges = next;
  }

  /** An agent banner of this change is in view. */
  bannerOn(changeId: string | undefined): boolean {
    return !!changeId && (this.bannerChanges[changeId] ?? 0) > 0;
  }

  async load() {
    try {
      this.current = await must(api.GET('/focus'));
    } catch {
      this.current = null;
    }
  }

  #apply(f: Focus) {
    const before = this.current;
    this.current = f;
    if (!f.route) return;
    // Only follow moves made elsewhere (another tab, an agent).
    if (f.route === router.current || f.route === before?.route) return;
    const agent = this.#agentName(f.by);
    if (agent) this.lastMove = { by: agent, route: f.route, at: Date.now(), followed: this.follow };
    if (this.follow) router.navigate(f.route);
  }

  /** "dennis/agent" when an agent moved the focus, else undefined (another tab). */
  #agentName(by: string | undefined): string | undefined {
    if (!by) return undefined;
    const p = this.presence[by];
    if (p) return p.kind === 'human' ? undefined : p.actsFor ? `${p.actsFor}/agent` : p.actor;
    const m = /^(.+)-agent$/.exec(by);
    return m ? `${m[1]}/agent` : undefined;
  }

  /** Stop moving the screen with agents (the cue offers Resume). */
  pause() {
    this.follow = false;
  }

  /** Follow again, and go where the agent is now. */
  resume() {
    this.follow = true;
    const r = this.current?.route;
    if (this.lastMove && !this.lastMove.followed && r && r !== router.current) {
      this.lastMove = { ...this.lastMove, followed: true, at: Date.now() };
      router.navigate(r);
    }
  }

  /** Go where the agent is, once, while following stays paused. */
  look() {
    const r = this.lastMove?.route;
    if (r && r !== router.current) router.navigate(r);
    this.lastMove = null;
  }

  /**
   * Move the focus. The update replaces the focus (fields left out are
   * cleared, except pinned), so send all that the user looks at. Calls are
   * debounced (250 ms) and merged, so pages can call it on every navigation
   * and cursor move. Safe to call in an $effect: it does not track state.
   */
  set(u: FocusUpdate) {
    untrack(() => {
      const pinned = u.pinned ?? this.#pending?.pinned ?? this.current?.pinned;
      this.#pending = { ...(this.#pending ?? {}), ...u, pinned };
    });
    clearTimeout(this.#timer);
    this.#timer = setTimeout(() => void this.#flush(), 250);
  }

  async #flush() {
    const body = this.#pending;
    this.#pending = null;
    if (!body) return;
    try {
      this.current = await must(api.PUT('/focus', { body }));
    } catch (e) {
      console.warn('focus', e);
    }
  }

  /** Accept a suggested move of an agent. */
  acceptSuggestion() {
    const s = this.suggestion?.suggest;
    this.suggestion = null;
    if (!s) return;
    if (s.route) router.navigate(s.route);
    this.set(s);
  }

  dismissSuggestion() {
    this.suggestion = null;
  }
}

export const focus = new FocusStore();
