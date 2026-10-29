// Short feedback messages for actions (push, rebase, merge, ...).
import { errorText } from '../api/client';

export type Tone = 'ok' | 'bad' | 'info' | 'warn';

export interface Toast {
  id: number;
  tone: Tone;
  text: string;
  detail?: string;
  link?: { label: string; href: string };
}

class Toasts {
  list = $state<Toast[]>([]);
  #n = 0;

  push(t: Omit<Toast, 'id'>, ms = t.tone === 'bad' ? 9000 : 5000): number {
    const id = ++this.#n;
    this.list = [...this.list, { ...t, id }].slice(-4);
    if (ms > 0) setTimeout(() => this.dismiss(id), ms);
    return id;
  }

  ok(text: string, detail?: string, link?: Toast['link']) {
    return this.push({ tone: 'ok', text, detail, link });
  }

  info(text: string, detail?: string, link?: Toast['link']) {
    return this.push({ tone: 'info', text, detail, link });
  }

  /** An error from must() or fetch, with a short lead text. */
  error(lead: string, e: unknown) {
    return this.push({ tone: 'bad', text: lead, detail: errorText(e) });
  }

  dismiss(id: number) {
    this.list = this.list.filter((t) => t.id !== id);
  }
}

export const toasts = new Toasts();
