// Formatting helpers for dates, shas and ids. No imports: unit tested with
// node --test (web/unit).

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

const pad = (n: number) => String(n).padStart(2, '0');

function parse(d: string | Date | undefined | null): Date | null {
  if (!d) return null;
  const t = d instanceof Date ? d : new Date(d);
  return Number.isNaN(t.getTime()) ? null : t;
}

/** "14 Oct" (adds the year when it is not the year of now). */
export function shortDate(d: string | Date | undefined | null, now: Date = new Date()): string {
  const t = parse(d);
  if (!t) return '';
  const s = `${t.getDate()} ${MONTHS[t.getMonth()]}`;
  return t.getFullYear() === now.getFullYear() ? s : `${s} ${t.getFullYear()}`;
}

/** "2026-10-14 10:30" in local time. */
export function dateTime(d: string | Date | undefined | null): string {
  const t = parse(d);
  if (!t) return '';
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}`;
}

/** "17:40" in local time. */
export function clock(d: string | Date | undefined | null): string {
  const t = parse(d);
  return t ? `${pad(t.getHours())}:${pad(t.getMinutes())}` : '';
}

/** "ends 17:40" for today, "ends tomorrow 01:40", "ended" when past. */
export function endsAt(d: string | Date | undefined | null, now: Date = new Date()): string {
  const t = parse(d);
  if (!t) return '';
  if (t.getTime() <= now.getTime()) return 'ended';
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(t) - day(now)) / 86_400_000);
  if (diff === 0) return `ends ${clock(t)}`;
  if (diff === 1) return `ends tomorrow ${clock(t)}`;
  return `ends ${shortDate(t, now)} ${clock(t)}`;
}

/** "3 min ago", "2 h ago", "5 d ago", "just now". */
export function ago(d: string | Date | undefined | null, now: Date = new Date()): string {
  const t = parse(d);
  if (!t) return '';
  const s = Math.round((now.getTime() - t.getTime()) / 1000);
  if (s < 45) return 'just now';
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 36) return `${h} h ago`;
  return `${Math.round(h / 24)} d ago`;
}

/** Short sha: 7 characters. */
export const short = (sha: string | undefined | null) => (sha ? sha.slice(0, 7) : '');

/** Change id (c-xxxxxx) or not (a commit sha of a direct commit). */
export const isChangeId = (s: string | undefined | null) => !!s && /^c-[a-z0-9]{6}$/.test(s);

/** "1.2 s" or "480 ms". */
export function duration(ms: number | undefined | null): string {
  if (ms === undefined || ms === null) return '';
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  return `${m}m ${Math.round(s - m * 60)}s`;
}
