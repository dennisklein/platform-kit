// One EventSource on /api/v1/events for the whole app. Components
// subscribe with events.on(type, fn) and get the Event data payload.
// The browser sends Last-Event-ID on reconnect; the bench replays.
import type { EventType } from '../api/types';

export interface BenchEvent<T = unknown> {
  id: number;
  type: EventType;
  time: string;
  data: T;
}

type Handler = (data: any, ev: BenchEvent) => void;

const TYPES: EventType[] = [
  'change.created',
  'change.updated',
  'lab.step',
  'lab.finished',
  'render.updated',
  'focus.changed',
  'forge.updated',
  // The forge state was rebuilt (pk forge-init --reset): clients reload.
  'forge.reset',
  'flux.updated',
  'presence',
  'suggestion.created',
  // Live progress of a chat answer (only to the person who asked).
  'chat.step',
  'chat.delta',
];

class Events {
  connected = $state(false);
  lastId = $state(0);
  /** Last few events, newest first, for debugging and activity lists. */
  recent = $state<BenchEvent[]>([]);

  #src: EventSource | null = null;
  #handlers = new Map<string, Set<Handler>>();
  #lost = false;
  #reconnect = new Set<() => void>();

  start(url = '/api/v1/events') {
    if (this.#src) return;
    const src = new EventSource(url, { withCredentials: true });
    this.#src = src;
    src.onopen = () => {
      this.connected = true;
      // Back after a bench restart: events may be lost (the replay buffer
      // starts empty), so stores reload.
      if (this.#lost) for (const fn of this.#reconnect) fn();
      this.#lost = false;
    };
    src.onerror = () => {
      this.connected = src.readyState === EventSource.OPEN;
      if (!this.connected) this.#lost = true;
    };
    for (const t of TYPES) {
      src.addEventListener(t, (m) => this.#dispatch(m as MessageEvent));
    }
  }

  stop() {
    this.#src?.close();
    this.#src = null;
    this.connected = false;
  }

  #dispatch(m: MessageEvent) {
    let ev: BenchEvent;
    try {
      ev = JSON.parse(m.data);
    } catch {
      return;
    }
    this.lastId = ev.id;
    // Live chat progress is not activity worth listing.
    if (ev.type !== 'chat.step' && ev.type !== 'chat.delta') this.recent = [ev, ...this.recent].slice(0, 50);
    for (const key of [ev.type, '*']) {
      for (const h of this.#handlers.get(key) ?? []) {
        try {
          h(ev.data, ev);
        } catch (e) {
          console.error('event handler', ev.type, e);
        }
      }
    }
  }

  /** Call fn when the stream comes back after it was lost. */
  onReconnect(fn: () => void): () => void {
    this.#reconnect.add(fn);
    return () => this.#reconnect.delete(fn);
  }

  /**
   * Call fn for each event of a type ("*" for all). Returns the unsubscribe
   * function, so in a component: $effect(() => events.on('lab.step', fn)).
   */
  on<T = any>(type: EventType | '*', fn: (data: T, ev: BenchEvent<T>) => void): () => void {
    let set = this.#handlers.get(type);
    if (!set) this.#handlers.set(type, (set = new Set()));
    set.add(fn as Handler);
    return () => set!.delete(fn as Handler);
  }
}

export const events = new Events();
