// MCP tool calls of agents seen in this tab (presence events), newest
// first. The agents page and the side panel show them like the harness
// transcript. A call the bench denies (an agent tried to approve or merge)
// raises a red toast wherever the admin looks.
import type { Presence } from '../api/types';
import { events } from './events.svelte';
import { toasts } from './toasts.svelte';
import { agentName, callArgs, reasonText, toCall, type McpCall } from '../f/mcp';

/** Older presence (a replay after a reconnect) does not raise a toast. */
const FRESH_MS = 30_000;
const NEVER = new Set(['pk_change_approve', 'pk_change_merge']);

class McpCalls {
  list = $state<McpCall[]>([]);
}

export const mcpCalls = new McpCalls();

function announceDenied(c: McpCall) {
  const at = Date.parse(c.at);
  if (Number.isFinite(at) && Date.now() - at > FRESH_MS) return;
  const args = callArgs(c);
  const why = reasonText(c.reason);
  toasts.push(
    {
      tone: 'bad',
      text: `Denied: ${agentName(c)} tried ${c.tool}${args ? ` on ${args}` : ''}`,
      detail: `${why || 'The bench refused the call'}.${c.tool && NEVER.has(c.tool) ? ' People do.' : ''}`,
    },
    10_000,
  );
}

events.on<Presence>('presence', (p) => {
  if (!p?.tool) return;
  const c = toCall(p);
  mcpCalls.list = [c, ...mcpCalls.list].slice(0, 40);
  if (c.denied) announceDenied(c);
});
