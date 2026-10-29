// Small formatters for the change workflow screens. No imports with
// runtime effect, so node --test can load this file directly.

/** First 7 characters of a sha. */
export function shortSha(sha: string | undefined | null): string {
  return sha ? sha.slice(0, 7) : '';
}

/** "run-3" from "c-k7m2qz/run-3". */
export function runLabel(run: string | undefined | null): string {
  if (!run) return '';
  const i = run.lastIndexOf('/');
  return i < 0 ? run : run.slice(i + 1);
}

/** Path segment for a lab run id ("c-k7m2qz/run-1" → "c-k7m2qz%2Frun-1"). */
export function runSegment(run: string): string {
  return encodeURIComponent(run);
}

/** "6m 40s", "25s", "1h 2m" from milliseconds. */
export function duration(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || !Number.isFinite(ms) || ms < 0) return '';
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 ? `${m}m ${s % 60}s` : `${m}m`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h}h ${m % 60}m` : `${h}h`;
}

/** "just now", "5 min ago", "3 h ago", "2 days ago", or a date. */
export function timeAgo(at: string | undefined | null, now: number = Date.now()): string {
  if (!at) return '';
  const t = Date.parse(at);
  if (Number.isNaN(t)) return '';
  const s = Math.round((now - t) / 1000);
  if (s < 45) return 'just now';
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.round(h / 24);
  if (d < 14) return d === 1 ? 'yesterday' : `${d} days ago`;
  return new Date(t).toISOString().slice(0, 10);
}

/** "17:40" local time. */
export function clock(at: string | undefined | null): string {
  if (!at) return '';
  const t = new Date(at);
  if (Number.isNaN(t.getTime())) return '';
  return t.toTimeString().slice(0, 5);
}

/** "1 file", "3 files". */
export function plural(n: number, one: string, many = one + 's'): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** Last path element. */
export function baseName(path: string): string {
  const i = path.lastIndexOf('/');
  return i < 0 ? path : path.slice(i + 1);
}

/** "~/pk/c-k7m2qz" style: shorten the home directory. */
export function tildePath(path: string | undefined | null): string {
  if (!path) return '';
  return path.replace(/^\/(?:home\/[^/]+|root)(?=\/|$)/, '~');
}

/** Short form of a long path for status lines: "…/wt/c-k7m2qz". */
export function shortPath(path: string | undefined | null, keep = 2): string {
  const t = tildePath(path);
  const parts = t.split('/');
  return parts.length > keep + 2 ? '…/' + parts.slice(-keep).join('/') : t;
}

/** Change id from a branch "change/c-k7m2qz-add-..." (stack targets). */
export function changeIdOfBranch(branch: string | undefined | null): string | undefined {
  const m = /^change\/(c-[a-z0-9]{6})(?:-|$)/.exec(branch ?? '');
  return m ? m[1] : undefined;
}
