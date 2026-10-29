// Freeform chat of this browser tab (side panel and chat page share it).
// The bench answers as the logged-in human; edits come back as suggestions
// in the change the user works on.
import { api, must, errorText } from '../api/client';
import type { ChatAnswer } from '../api/ftypes';
import { focus } from './focus.svelte';
import { session } from './me.svelte';
import { work } from './context.svelte';

export interface ChatEntry {
  id: number;
  role: 'user' | 'assistant';
  content: string;
  answer?: ChatAnswer;
  error?: string;
  changeId?: string;
  at: string;
}

const KEY = 'pk.chat';
const MAX_HISTORY = 12;

function restore(): ChatEntry[] {
  try {
    const v = JSON.parse(sessionStorage.getItem(KEY) ?? '[]');
    return Array.isArray(v) ? v : [];
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

  /** The change the next question is about. */
  get changeId(): string | undefined {
    if (this.picked === null) return undefined;
    return this.picked ?? focus.current?.changeId ?? work.changeId;
  }

  #save() {
    try {
      sessionStorage.setItem(KEY, JSON.stringify(this.entries.slice(-40)));
    } catch {
      // private mode
    }
  }

  async send(text: string) {
    const content = text.trim();
    if (!content || this.busy) return;
    const changeId = this.changeId;
    this.entries = [...this.entries, { id: ++this.#n, role: 'user', content, changeId, at: new Date().toISOString() }];
    this.#save();
    this.busy = true;
    const messages = this.entries
      .filter((e) => !e.error)
      .slice(-MAX_HISTORY)
      .map((e) => ({ role: e.role, content: e.role === 'assistant' ? (e.answer?.text ?? e.content) : e.content }));
    try {
      const answer = await must(
        api.POST('/chat', {
          body: { messages, changeId, path: changeId && focus.current?.changeId === changeId ? focus.current?.path : undefined },
        }),
      );
      this.entries = [
        ...this.entries,
        { id: ++this.#n, role: 'assistant', content: answer.text, answer, changeId, at: new Date().toISOString() },
      ];
      session.noteAnswer(answer);
    } catch (e) {
      this.entries = [
        ...this.entries,
        { id: ++this.#n, role: 'assistant', content: '', error: errorText(e), changeId, at: new Date().toISOString() },
      ];
    } finally {
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
    this.entries = [];
    this.#save();
  }
}

export const chat = new ChatStore();
