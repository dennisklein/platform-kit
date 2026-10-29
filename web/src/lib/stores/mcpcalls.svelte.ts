// MCP tool calls of agents (presence events), newest first. The agents page
// and the side panel show them like the harness transcript. The bench
// recalls the recent calls when a page connects, so a reload keeps them. A
// call the bench denies (an agent tried to approve or merge) raises a red
// toast, unless an agent banner of that change shows the denial already.
import type { Presence } from '../api/types';
import { events } from './events.svelte';
import { toasts } from './toasts.svelte';
import { focus } from './focus.svelte';
import { agentName, callArgs, reasonText, sameCall, toCall, type McpCall } from '../f/mcp';

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
  // The red banner on the page says it, with the reason.
  if (focus.bannerOn(c.changeId)) return;
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

events.on<Presence>('presence', (p, ev) => {
  if (!p?.tool) return;
  const c = toCall(p);
  if (mcpCalls.list.some((x) => sameCall(x, c))) return;
  // Recalled calls come oldest first, live ones newest: keep newest first.
  mcpCalls.list = [c, ...mcpCalls.list].sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : 0)).slice(0, 40);
  if (c.denied && !ev.replay) announceDenied(c);
});
