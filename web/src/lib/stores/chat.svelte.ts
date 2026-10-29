// Freeform chat of this browser tab (side panel and chat page share it).
// The bench answers as the logged-in human; edits come back as suggestions
// in the change the user works on. While an answer forms, the bench streams
// its steps and text (chat.step, chat.delta) into a live entry; the final
// response replaces it.
import { api, must, errorText } from '../api/client';
import type { ChatAnswer } from '../api/ftypes';
import { focus } from './focus.svelte';
import { session } from './me.svelte';
import { work } from './context.svelte';
import { events } from './events.svelte';
import {
  applyDelta,
  applyStep,
  newRequestId,
  type DeltaEvent,
  type LiveState,
  type StepEvent,
} from '../components/chat/live';

export interface ChatEntry {
  id: number;
  role: 'user' | 'assistant';
  content: string;
  answer?: ChatAnswer;
  error?: string;
  /** The answer that is forming (steps and streamed text). */
  live?: LiveState;
  /** The asker stopped the answer; content is the text so far. */
  stopped?: boolean;
  changeId?: string;
  at: string;
}

const KEY = 'pk.chat';
const MAX_HISTORY = 12;

function restore(): ChatEntry[] {
  try {
    const v = JSON.parse(sessionStorage.getItem(KEY) ?? '[]');
    // An answer that was forming when the tab reloaded is lost.
    return Array.isArray(v) ? v.filter((e: ChatEntry) => !e.live) : [];
  } catch {
    return [];
  }
}

class ChatStore {
  entries = $state<ChatEntry[]>(restore());
  busy = $state(false);
  /** Change id the user picked for the chat; null = no change, undefined = follow the work context. */
  picked = $state<string | null | undefined>(undefined);
  #n = this.entries.reduce((m, e) => Math.max(m, e.id), 0);
  #abort: AbortController | null = null;
  #liveId = 0;

  constructor() {
    events.on<StepEvent>('chat.step', (d) => this.#onLive(d.requestId, (l) => ({ ...l, steps: applyStep(l.steps, d.step) })));
    events.on<DeltaEvent>('chat.delta', (d) => this.#onLive(d.requestId, (l) => ({ ...l, text: applyDelta(l.text, d) })));
  }

  /** The change the next question is about. */
  get changeId(): string | undefined {
    if (this.picked === null) return undefined;
    return this.picked ?? focus.current?.changeId ?? work.changeId;
  }

  /** The newest answer (for Regenerate). */
  get lastAnswerId(): number | undefined {
    const last = this.entries[this.entries.length - 1];
    return last?.role === 'assistant' && !last.live ? last.id : undefined;
  }

  #onLive(requestId: string, fn: (l: LiveState) => LiveState) {
    const e = this.entries.find((x) => x.id === this.#liveId);
    if (!e?.live || e.live.requestId !== requestId) return;
    e.live = fn(e.live);
  }

  #save() {
    try {
      sessionStorage.setItem(KEY, JSON.stringify(this.entries.filter((e) => !e.live).slice(-40)));
    } catch {
      // private mode
    }
  }

  /** Ask a question. Returns false while an answer forms (the caller keeps the draft). */
  send(text: string): boolean {
    const content = text.trim();
    if (!content || this.busy) return false;
    const changeId = this.changeId;
    this.entries = [...this.entries, { id: ++this.#n, role: 'user', content, changeId, at: new Date().toISOString() }];
    void this.#ask(changeId);
    return true;
  }

  /** Ask the question of the newest answer again; the new answer replaces it. */
  regenerate(): boolean {
    if (this.busy || this.lastAnswerId === undefined) return false;
    const last = this.entries[this.entries.length - 1];
    this.entries = this.entries.slice(0, -1);
    void this.#ask(last.changeId);
    return true;
  }

  /** Stop the answer that is forming; the text so far stays. */
  stop() {
    this.#abort?.abort();
  }

  async #ask(changeId: string | undefined) {
    this.#save();
    this.busy = true;
    const messages = this.entries
      .filter((e) => !e.error && !e.stopped)
      .slice(-MAX_HISTORY)
      .map((e) => ({ role: e.role, content: e.role === 'assistant' ? (e.answer?.text ?? e.content) : e.content }));
    const requestId = newRequestId();
    const id = ++this.#n;
    this.#liveId = id;
    this.entries = [
      ...this.entries,
      { id, role: 'assistant', content: '', changeId, at: new Date().toISOString(), live: { requestId, steps: [], text: '', startedAt: Date.now() } },
    ];
    const ctl = new AbortController();
    this.#abort = ctl;
    const replace = (e: Partial<ChatEntry>) => {
      this.entries = this.entries.map((x) => (x.id === id ? { ...x, ...e, live: undefined } : x));
    };
    try {
      const answer = await must(
        api.POST('/chat', {
          body: {
            messages,
            changeId,
            requestId,
            path: changeId && focus.current?.changeId === changeId ? focus.current?.path : undefined,
          },
          signal: ctl.signal,
        }),
      );
      replace({ content: answer.text, answer });
      session.noteAnswer(answer);
    } catch (e) {
      const live = this.entries.find((x) => x.id === id)?.live;
      if (ctl.signal.aborted) replace({ content: live?.text ?? '', stopped: true });
      else replace({ content: '', error: errorText(e) });
    } finally {
      this.#abort = null;
      this.#liveId = 0;
      this.busy = false;
      this.#save();
    }
  }

  /** Keep a decided suggestion in the stored conversation. */
  update(entryId: number, answer: ChatAnswer) {
    this.entries = this.entries.map((e) => (e.id === entryId ? { ...e, answer } : e));
    this.#save();
  }

  clear() {
    this.stop();
    this.entries = [];
    this.#save();
  }
}

export const chat = new ChatStore();
