// One MCP tool call of an agent, from a presence event.
import type { Presence } from '../api/types';

/** Tools the bench always refuses for agents. */
const NEVER = new Set(['pk_change_approve', 'pk_change_merge']);

export interface McpCall extends Presence {
  failed: boolean;
  /** The bench refused the call (403), for example an approve. */
  denied: boolean;
  /** Short reason for a failed call, when it is known. */
  reason?: string;
}

// "pk_change_approve c-k7m2qz (failed: denied: agents never approve or merge)"
// or, from older benches, "… (failed)".
const FAILED = / \(failed(?:: (.*))?\)$/;

export function toCall(p: Presence): McpCall {
  const raw = p.text ?? '';
  const m = FAILED.exec(raw);
  const failed = !!m;
  const text = (m ? raw.slice(0, m.index) : raw) || p.tool || '';
  let reason = m?.[1]?.trim() || undefined;
  if (failed && !reason) reason = p.tool && NEVER.has(p.tool) ? 'denied: agents never approve or merge' : 'failed';
  const denied = failed && (!!reason?.startsWith('denied') || (!!p.tool && NEVER.has(p.tool)));
  return { ...p, text, failed, denied, reason };
}

/** "dennis/agent" for an agent that acts for dennis. */
export function agentName(p: Pick<Presence, 'actor' | 'actsFor'>): string {
  return p.actsFor ? `${p.actsFor}/agent` : p.actor;
}

/** The arguments part of a call line: "pk_file_write cn-037.yaml" → "cn-037.yaml". */
export function callArgs(c: Pick<McpCall, 'text' | 'tool'>): string {
  const t = c.text ?? '';
  return c.tool && t.startsWith(c.tool) ? t.slice(c.tool.length).trim() : t;
}

/** A UI route in words, for "Following dennis/agent → review of c-k7m2qz". */
export function routeLabel(route: string): string {
  const [path, qs = ''] = route.split('?', 2);
  const q = new URLSearchParams(qs);
  const file = (q.get('path') ?? '').split('/').pop();
  let m = /^\/changes\/(c-[a-z0-9]+)(?:\/(review|files|lab|history))?\/?$/.exec(path);
  if (m) {
    const [, id, view] = m;
    if (!view) return `change ${id}`;
    if (view === 'files') return file ? `${file} in ${id}` : `files of ${id}`;
    if (view === 'review' && q.get('advisor')) return `review of ${id} · advisor`;
    return `${view} of ${id}`;
  }
  m = /^\/assets\/racks\/([^/]+)$/.exec(path);
  if (m) return `rack ${decodeURIComponent(m[1])}`;
  m = /^\/assets\/([^/]+)\/([^/]+)$/.exec(path);
  if (m) return `${decodeURIComponent(m[1])} ${decodeURIComponent(m[2])}`;
  if (path === '/kb/article') return file ? `knowledge: ${file}` : 'knowledge';
  return path.replace(/^\//, '') || 'home';
}

/** The reason without its "denied: " lead, capitalized: "Agents never approve or merge". */
export function reasonText(reason: string | undefined): string {
  const r = (reason ?? '').replace(/^denied:\s*/i, '').trim();
  return r ? r[0].toUpperCase() + r.slice(1) : '';
}

/** How long the banner stays red after a denied call. */
export const DENIED_MS = 15_000;

/** Milliseconds the red denied state has left for a call made at `at` (0: over). */
export function deniedLeft(at: string, now: number, ms = DENIED_MS): number {
  const t = Date.parse(at);
  if (!Number.isFinite(t)) return ms;
  return Math.max(0, t + ms - now);
}

/** The calm line after a denial: "Agent stopped: merging needs a human". */
export function stoppedText(c: Pick<McpCall, 'tool'>): string {
  if (c.tool === 'pk_change_merge') return 'Agent stopped: merging needs a human';
  if (c.tool === 'pk_change_approve') return 'Agent stopped: approving needs a human';
  return `Agent stopped: the bench refused ${c.tool ?? 'the call'}`;
}

type CallKey = Pick<McpCall, 'actor' | 'at' | 'text' | 'tool'>;

/** The same call seen twice (recalled after a reload, then again on a reconnect). */
export function sameCall(a: CallKey, b: CallKey): boolean {
  return a.actor === b.actor && a.at === b.at && a.tool === b.tool && a.text === b.text;
}
