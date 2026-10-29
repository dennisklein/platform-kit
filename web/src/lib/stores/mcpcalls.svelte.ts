// MCP tool calls of agents seen in this tab (presence events), newest
// first. The agents page shows them like the harness transcript.
import type { Presence } from '../api/types';
import { events } from './events.svelte';
import { toCall, type McpCall } from '../f/mcp';

class McpCalls {
  list = $state<McpCall[]>([]);
}

export const mcpCalls = new McpCalls();

events.on<Presence>('presence', (p) => {
  if (!p?.tool) return;
  mcpCalls.list = [toCall(p), ...mcpCalls.list].slice(0, 40);
});
