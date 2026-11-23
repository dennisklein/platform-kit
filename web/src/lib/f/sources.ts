// Link targets for LLM sources (advisor points, chat answers, log entries).
// No imports: unit tested with node --test (web/unit).

export interface SourceLike {
  label: string;
  ref?: string;
  kind: string;
}

const CHANGE = /^c-[a-z0-9]{6}$/;
const SHA = /^[0-9a-f]{7,40}$/;

/** Split "path:21" into path and line. */
export function pathLine(ref: string): { path: string; line?: number } {
  const m = /^(.*?):(\d+)$/.exec(ref);
  return m ? { path: m[1], line: Number(m[2]) } : { path: ref };
}

/**
 * App route ("/history?q=c-2hd7vw") or external URL for a source. changeId is
 * the change the source belongs to: files open in its worktree.
 */
export function sourceRoute(s: SourceLike, changeId?: string): string | null {
  const ref = (s.ref ?? '').trim();
  if (/^https?:\/\//.test(ref)) return ref;
  const q = (path: string, params: Record<string, string | undefined>) => {
    const u = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) if (v) u.set(k, v);
    const t = u.toString();
    return t ? `${path}?${t}` : path;
  };
  switch (s.kind) {
    case 'trailer':
    case 'change':
      if (CHANGE.test(ref)) return s.kind === 'change' ? `/changes/${ref}` : q('/history', { q: ref });
      if (SHA.test(ref)) return `/history/${ref}`;
      return ref ? q('/history', { q: ref }) : null;
    case 'mr':
      return ref ? q('/history', { q: `Reviewed-in: ${ref}` }) : null;
    case 'history':
      return SHA.test(ref) ? `/history/${ref}` : ref ? q('/history', { q: ref }) : null;
    case 'kb':
      return ref ? q('/kb/article', { path: ref }) : null;
    case 'lab':
      if (!ref) return null;
      if (ref.endsWith('.md')) return q('/kb/article', { path: ref });
      return `/labs/runs/${encodeURIComponent(ref)}`;
    case 'impact':
      return CHANGE.test(ref) ? q(`/changes/${ref}/review`, { tab: 'impact' }) : changeId ? q(`/changes/${changeId}/review`, { tab: 'impact' }) : null;
    case 'file':
    case 'policy': {
      if (!ref) return null;
      const { path, line } = pathLine(ref);
      const params = { path, line: line ? String(line) : undefined };
      return changeId ? q(`/changes/${changeId}/files`, params) : q('/files', params);
    }
    case 'entity': {
      if (!ref) return null;
      const [kind, name] = ref.includes('/') ? ref.split('/', 2) : ['', ref];
      if (kind && name) return kind === 'Rack' ? `/assets/racks/${encodeURIComponent(name)}` : `/assets/${kind}/${encodeURIComponent(name)}`;
      return q('/assets', { q: ref });
    }
    case 'search':
      return ref ? q('/kb', { q: ref }) : null;
    default:
      return null;
  }
}
