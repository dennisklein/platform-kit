// One MCP tool call of an agent, from a presence event.
import type { Presence } from '../api/types';

/** Tools the bench always refuses for agents. */
const NEVER = new Set(['pk_change_approve', 'pk_change_merge']);

export interface McpCall extends Presence {
  failed: boolean;
  /** Short reason for a failed call, when it is known. */
  reason?: string;
}

export function toCall(p: Presence): McpCall {
  const failed = / \(failed\)$/.test(p.text ?? '');
  const text = (p.text ?? p.tool ?? '').replace(/ \(failed\)$/, '');
  return {
    ...p,
    text,
    failed,
    reason: failed && p.tool && NEVER.has(p.tool) ? 'denied: agents never approve or merge' : failed ? 'failed' : undefined,
  };
}
