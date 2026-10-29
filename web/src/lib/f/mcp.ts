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

/** The reason without its "denied: " lead, capitalized: "Agents never approve or merge". */
export function reasonText(reason: string | undefined): string {
  const r = (reason ?? '').replace(/^denied:\s*/i, '').trim();
  return r ? r[0].toUpperCase() + r.slice(1) : '';
}
