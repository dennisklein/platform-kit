// Agent approval requests of the Agents page.
import type { AgentMode } from '../api/ftypes';

/** The durations the page offers (the bench takes 1 to 12 h). */
export const HOURS = [1, 2, 4, 6, 8, 10, 12] as const;

export interface ApproveBody {
  hours: number;
  mode: AgentMode;
}

function clampHours(h: number | string): number {
  const n = Math.round(Number(h));
  return Number.isFinite(n) ? Math.min(12, Math.max(1, n)) : 8;
}

/** First approval: the duration and mode chosen on the page. */
export function approveBody(hours: number | string, mode: AgentMode): ApproveBody {
  return { hours: clampHours(hours), mode };
}

/** New token for a running grant: the chosen duration, the grant's own mode. */
export function newTokenBody(g: { mode: AgentMode }, hours: number | string): ApproveBody {
  return { hours: clampHours(hours), mode: g.mode };
}
