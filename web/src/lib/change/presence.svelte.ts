// Agent presence per change (from presence events and the change summary)
// and the agent grants for the "agent here" banner.
import { api, must } from '../api/client';
import type { AgentGrant, Presence } from '../api/types';
import { focus } from '../stores/focus.svelte';

/** Presence older than this is not shown as "working here". */
export const PRESENCE_TTL = 3 * 60_000;

class Clock {
  now = $state(Date.now());
  #t: ReturnType<typeof setInterval> | undefined;
  start() {
    if (this.#t) return;
    this.#t = setInterval(() => (this.now = Date.now()), 10_000);
  }
}

export const clock = new Clock();

/** The newest recent presence of an agent on this change. */
export function presenceFor(changeId: string | undefined, fromSummary?: Presence): Presence | undefined {
  clock.start();
  if (!changeId) return undefined;
  const now = clock.now;
  let best: Presence | undefined;
  for (const p of [...Object.values(focus.presence), ...(fromSummary ? [fromSummary] : [])]) {
    if (p.changeId !== changeId || p.kind === 'human') continue;
    const at = Date.parse(p.at);
    if (!Number.isFinite(at) || now - at > PRESENCE_TTL) continue;
    if (!best || p.at > best.at) best = p;
  }
  return best;
}

class Grants {
  list = $state<AgentGrant[]>([]);
  #loaded = 0;
  async load(force = false) {
    if (!force && Date.now() - this.#loaded < 30_000) return;
    this.#loaded = Date.now();
    try {
      this.list = await must(api.GET('/agents'));
    } catch {
      // not allowed or not built: no banner details
    }
  }
  of(id: string | undefined): AgentGrant | undefined {
    return this.list.find((g) => g.id === id);
  }
}

export const grants = new Grants();
