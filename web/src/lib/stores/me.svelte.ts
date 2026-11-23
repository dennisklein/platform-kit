// The identity of this session and the bench status (top bar pills).
import { api, must } from '../api/client';
import type { BenchStatus, Me } from '../api/types';
import { mainRewritten } from '../f/reset';
import { needsStatusCheck, type AnswerLike } from '../f/llm';

class Session {
  me = $state<Me | null>(null);
  status = $state<BenchStatus | null>(null);
  error = $state<string | null>(null);
  #timer: ReturnType<typeof setInterval> | undefined;

  async load() {
    try {
      this.me = await must(api.GET('/me'));
      this.error = null;
    } catch (e) {
      this.me = null;
      this.error = e instanceof Error ? e.message : String(e);
    }
  }

  async refreshStatus() {
    try {
      const prev = this.status?.main;
      this.status = await must(api.GET('/status'));
      // main moved back in time: the forge was rebuilt (make reset).
      if (mainRewritten(prev, this.status.main)) window.dispatchEvent(new CustomEvent('pk:forge-reset'));
    } catch {
      // keep the last status; pills show it as stale
    }
  }

  /** An answer from data instead of the model: refresh the pills now, not at the next poll. */
  noteAnswer(a: AnswerLike | null | undefined) {
    if (needsStatusCheck(a)) void this.refreshStatus();
  }

  /** Poll the status every 10 s. */
  start() {
    void this.load();
    void this.refreshStatus();
    clearInterval(this.#timer);
    this.#timer = setInterval(() => void this.refreshStatus(), 10_000);
    window.addEventListener('pk:unauthorized', () => (this.me = null));
  }

  get isAgent() {
    return this.me?.kind === 'agent';
  }
}

export const session = new Session();
