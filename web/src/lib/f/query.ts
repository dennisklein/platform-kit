// History search box: "entity:cn-003 author:jonas BGP" <-> filter params.
// No imports: unit tested with node --test (web/unit).

export const FILTER_KEYS = ['entity', 'path', 'author', 'kind'] as const;
export type FilterKey = (typeof FILTER_KEYS)[number];

export interface HistoryQuery {
  entity?: string;
  path?: string;
  author?: string;
  kind?: string;
  /** Free text (all words must match). */
  q?: string;
}

const TOKEN = /(\w+):(?:"([^"]*)"|(\S*))|"([^"]*)"|(\S+)/g;

/** Parse the search box text. Unknown "key:" prefixes stay free text. */
export function parseQuery(text: string): HistoryQuery {
  const out: HistoryQuery = {};
  const words: string[] = [];
  for (const m of text.matchAll(TOKEN)) {
    const [, key, quoted, bare, phrase, word] = m;
    if (key !== undefined) {
      const k = key.toLowerCase();
      const v = (quoted ?? bare ?? '').trim();
      if ((FILTER_KEYS as readonly string[]).includes(k)) {
        if (v) out[k as FilterKey] = v;
        continue;
      }
      words.push(m[0]);
      continue;
    }
    words.push(phrase ?? word ?? '');
  }
  const q = words.filter(Boolean).join(' ').trim();
  if (q) out.q = q;
  return out;
}

const quote = (v: string) => (/\s/.test(v) ? `"${v}"` : v);

/** Text for the search box from filter params (stable key order). */
export function formatQuery(q: HistoryQuery): string {
  const parts: string[] = [];
  for (const k of FILTER_KEYS) if (q[k]) parts.push(`${k}:${quote(q[k]!)}`);
  if (q.q) parts.push(q.q);
  return parts.join(' ');
}

/** Filter params from URL query params (?entity=cn-003&q=...). */
export function fromParams(p: URLSearchParams): HistoryQuery {
  // ?q= may carry "path:..." filters too (links built from the search text);
  // explicit params win.
  const out: HistoryQuery = parseQuery(p.get('q') ?? '');
  for (const k of FILTER_KEYS) {
    const v = p.get(k);
    if (v) out[k] = v;
  }
  return out;
}

/** URL query params for filter params (empty values left out). */
export function toParams(q: HistoryQuery): Record<string, string> {
  const out: Record<string, string> = {};
  for (const k of [...FILTER_KEYS, 'q'] as const) if (q[k]) out[k] = q[k]!;
  return out;
}
