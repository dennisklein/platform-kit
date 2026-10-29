// Cluster health and the landing page counts. Pure, tested with node --test.

export interface ClusterLike {
  reachable?: boolean;
  failing?: boolean;
  healthy: boolean;
  held?: boolean;
  applied?: { sha: string };
  verified?: { sha: string };
  pending?: { sha: string };
}

export type HealthTone = 'ok' | 'warn' | 'bad' | 'muted';
export interface Health {
  tone: HealthTone;
  label: string;
}

/** One word for a cluster: healthy, reconciling, verifying, held, failing, unreachable. */
export function clusterHealth(c: ClusterLike): Health {
  if (c.reachable === false) return { tone: 'muted', label: 'unreachable' };
  if (c.failing || !c.healthy) return { tone: 'bad', label: 'failing' };
  if (c.held) return { tone: 'warn', label: 'held' };
  if (c.pending && c.pending.sha !== c.applied?.sha) return { tone: 'warn', label: 'reconciling' };
  if (c.applied && c.verified?.sha !== c.applied.sha) return { tone: 'warn', label: 'verifying' };
  return { tone: 'ok', label: 'healthy' };
}

export interface ChangeLike {
  group: string;
  labState?: string;
}

export interface Counts {
  open: number;
  mine: number;
  others: number;
  review: number;
  bots: number;
  labFailed: number;
  labRunning: number;
}

/** Counts for the stat tiles of the Changes page (open changes only). */
export function changeCounts(list: ChangeLike[]): Counts {
  const n: Counts = { open: list.length, mine: 0, others: 0, review: 0, bots: 0, labFailed: 0, labRunning: 0 };
  for (const c of list) {
    if (c.group === 'mine') n.mine++;
    else if (c.group === 'review') n.review++;
    else if (c.group === 'bots') n.bots++;
    else n.others++;
    if (c.labState === 'fail') n.labFailed++;
    else if (c.labState === 'running') n.labRunning++;
  }
  return n;
}

/** The subtitle of the open-changes tile: the parts add up to the total. */
export function openBreakdown(n: Counts): string {
  const parts = [`${n.mine} mine`];
  // Short words: the line must fit a quarter of the projector width.
  if (n.review) parts.push(`${n.review} review`);
  if (n.others) parts.push(`${n.others} ${n.others === 1 ? 'other' : 'others'}`);
  if (n.bots) parts.push(`${n.bots} ${n.bots === 1 ? 'bot' : 'bots'}`);
  return parts.join(' · ');
}
