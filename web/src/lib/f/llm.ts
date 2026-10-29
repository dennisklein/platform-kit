// LLM labels: the top bar pill and the model line of answers. When the
// model is down the bench answers with its deterministic findings; the UI
// says so instead of naming the model.

export interface LlmStatusLike {
  reachable: boolean;
  mock: boolean;
  model: string;
  baseUrl?: string;
  detail?: string;
}

/** "llm: Get "http://…/models": dial tcp 172.17.0.1:11434: connect: connection refused" → "connection refused". */
export function offlineReason(detail?: string): string {
  if (!detail) return 'not reachable';
  const known = /(connection refused|no such host|i\/o timeout|context deadline exceeded|timeout|network is unreachable|connection reset|EOF|\b[45]\d\d\b[^:"]*)/i.exec(detail);
  if (known) return known[1].trim();
  const parts = detail.split(': ').map((s) => s.trim()).filter(Boolean);
  return parts[parts.length - 1] ?? detail;
}

export function llmPill(st: LlmStatusLike | undefined): { label: string; tone: '' | 'ok' | 'warn'; title: string } {
  if (!st) return { label: 'LLM', tone: '', title: '' };
  if (st.mock) return { label: 'LLM mock', tone: st.reachable ? 'ok' : 'warn', title: 'Deterministic mock model (no PK_LLM_BASE_URL)' };
  const at = `${st.model}${st.baseUrl ? ` at ${st.baseUrl}` : ''}`;
  if (!st.reachable) return { label: 'LLM offline', tone: 'warn', title: `${at}: ${offlineReason(st.detail)}. Answers use the deterministic findings.` };
  return { label: `LLM ${st.model}`, tone: 'ok', title: st.detail ?? at };
}

export interface AnswerLike {
  mock?: boolean;
  model?: string;
  /** Set by the bench when the model failed and the deterministic answer took over. */
  fallback?: boolean;
}

/**
 * The model line of an answer. The bench sets fallback when its data answered
 * instead of the model. offline: the model is down (status); for an answer
 * without the field (stored chat history) a non-mock answer then came from
 * the fallback.
 */
export function modelLabel(a: AnswerLike, offline = false): { text: string; title: string; fallback: boolean } {
  if (a.mock) return { text: 'mock model', title: 'Deterministic mock model (no LLM configured)', fallback: false };
  const fb = a.fallback ?? (offline || /fallback|offline/i.test(a.model ?? ''));
  if (fb)
    return {
      text: 'LLM offline · answer from data',
      title: `${a.model ?? 'the model'} did not answer (or gave no usable answer); this is the bench's own answer from its data`,
      fallback: true,
    };
  return { text: a.model ?? '', title: 'Model', fallback: false };
}

/** The answer came from data although a model is configured: check the LLM status now (top bar pill). */
export function needsStatusCheck(a: AnswerLike | null | undefined): boolean {
  return !!a && !a.mock && a.fallback === true;
}
