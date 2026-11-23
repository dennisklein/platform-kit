// Live chat answers: the bench streams the steps (chat.step) and the text
// (chat.delta) of an answer while it forms. These pure helpers fold the
// events into the live entry; the final POST /chat response replaces it.

export interface LiveStep {
  id: number;
  tool: string;
  label: string;
  summary?: string;
  state: 'running' | 'ok' | 'failed';
  ms?: number;
}

export interface LiveState {
  requestId: string;
  steps: LiveStep[];
  text: string;
  /** Date.now() when the question went out. */
  startedAt: number;
}

export interface StepEvent {
  requestId: string;
  step: LiveStep;
}

export interface DeltaEvent {
  requestId: string;
  text: string;
  reset?: boolean;
}

/** Add or update a step (events of one step come running, then ok or failed). */
export function applyStep(steps: LiveStep[], step: LiveStep): LiveStep[] {
  const i = steps.findIndex((s) => s.id === step.id);
  if (i < 0) return [...steps, step].sort((a, b) => a.id - b.id);
  // A late "running" never undoes a finished step.
  if (step.state === 'running' && steps[i].state !== 'running') return steps;
  const out = steps.slice();
  out[i] = step;
  return out;
}

/** Append streamed text, or start over on reset. */
export function applyDelta(text: string, ev: DeltaEvent): string {
  return (ev.reset ? '' : text) + (ev.text ?? '');
}

/**
 * Close what a half-streamed Markdown text leaves open, so it renders
 * cleanly while it grows: a code fence, inline code and bold.
 */
export function closeMarkdown(text: string): string {
  let out = text;
  const fences = (out.match(/^```/gm) ?? []).length;
  if (fences % 2 === 1) return out + '\n```';
  // Inline code and bold outside code fences.
  const body = out.replace(/```[\s\S]*?```/g, '');
  if ((body.match(/`/g) ?? []).length % 2 === 1) out = /`\s*$/.test(out) ? out.replace(/`\s*$/, '') : out + '`';
  const bold = (body.replace(/`[^`]*`/g, '').match(/\*\*/g) ?? []).length;
  if (bold % 2 === 1) out = /\*\*\s*$/.test(out) ? out.replace(/\*\*\s*$/, '') : out + '**';
  return out;
}

/** The chip text of a step: "history cn-003 → 4 merges". */
export function stepText(s: LiveStep): string {
  return s.summary ? `${s.label} → ${s.summary}` : s.label;
}

/** "Used 3 tools" / "Used 1 tool". */
export function usedTools(n: number): string {
  return `Used ${n} ${n === 1 ? 'tool' : 'tools'}`;
}

/** Seconds since start, for the "Thinking · 6 s" line. */
export function elapsed(startedAt: number, now: number): string {
  const s = Math.max(0, Math.floor((now - startedAt) / 1000));
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`;
}

/** A request id for one question. */
export function newRequestId(): string {
  const c = globalThis.crypto as Crypto | undefined;
  if (c?.randomUUID) return c.randomUUID();
  return `r-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}

/** After this long without text, a model answer offers the answer from project data. */
export const DATA_OFFER_MS = 8000;

/** Offer "Answer from project data now": a model answer that has no text after 8 s. */
export function offerData(l: Pick<LiveState, 'text' | 'startedAt'>, now: number, model: boolean): boolean {
  return model && !l.text && now - l.startedAt >= DATA_OFFER_MS;
}

/** The steps of a stopped answer: what still ran is marked stopped. */
export function stopSteps(steps: LiveStep[]): LiveStep[] {
  return steps.map((s) => (s.state === 'running' ? { ...s, state: 'failed', summary: 'stopped' } : s));
}

/** The model of an answer the secret guard gave (no model call). */
export const GUARD_MODEL = 'guard';

/** The model of a log entry the asker had answered from the project data
 *  ("Answer from project data now"): no model call, none failed. */
export const ON_REQUEST_MODEL = 'project-data';

/** A log entry answered from project data on request (older entries
 *  recorded it as an error of the model call). */
export function answeredOnRequest(e: { model?: string; error?: string }): boolean {
  return e.model === ON_REQUEST_MODEL || !!e.error?.startsWith('answered from project data on request');
}

/** A log entry whose model call failed (not one answered on request). */
export function modelCallFailed(e: { model?: string; error?: string }): boolean {
  return !!e.error && !answeredOnRequest(e);
}

export interface FootLabel {
  text: string;
  title: string;
  fallback: boolean;
}

/**
 * The model line under an answer. The secret guard says so; in the narrow
 * side panel the built-in label is short ("built-in"), the clock line says
 * why the data answered.
 */
export function footLabel(base: FootLabel, a: { model?: string; mock?: boolean; fallback?: boolean }, short: boolean): FootLabel {
  if (a.model === GUARD_MODEL)
    return { text: 'built-in · secret guard', title: 'The secret guard answered: no model saw the question', fallback: false };
  if (short && (a.mock || base.fallback)) return { ...base, text: 'built-in' };
  return base;
}
