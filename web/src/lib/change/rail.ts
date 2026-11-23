// Grouping, filtering and sorting of the change rail. Pure functions over
// ChangeSummary, tested with node --test.
import type { ChangeGroup, ChangeState, ChangeSummary } from '../api/types';
import { lifecycleLabel, lifecycleState } from './lifecycle.ts';

export type RailFilter = 'all' | ChangeGroup;
export type RailSort = 'updated' | 'state' | 'title';

export interface RailItem {
  c: ChangeSummary;
  /** Depth in a stack (0 = targets main). */
  depth: number;
}

export interface RailGroup {
  key: ChangeGroup | 'merged';
  label: string;
  items: RailItem[];
}

export const GROUPS: { key: ChangeGroup; label: string }[] = [
  { key: 'mine', label: 'My changes' },
  { key: 'others', label: 'Pushed by others' },
  { key: 'review', label: 'Waiting for my review' },
  { key: 'bots', label: 'Bots' },
];

const STATE_ORDER: Record<ChangeState, number> = {
  approved: 0,
  review: 1,
  lab: 2,
  draft: 3,
  merged: 4,
  deployed: 5,
  verified: 6,
  closed: 7,
};

function compare(sort: RailSort) {
  return (a: ChangeSummary, b: ChangeSummary): number => {
    if (sort === 'title') return a.title.localeCompare(b.title);
    if (sort === 'state') {
      const d = STATE_ORDER[a.state] - STATE_ORDER[b.state];
      if (d) return d;
    }
    return (b.updatedAt ?? b.createdAt ?? '').localeCompare(a.updatedAt ?? a.createdAt ?? '');
  };
}

/** Words of the query must all appear in id, title, author, branch or intent. */
export function matches(c: ChangeSummary, query: string): boolean {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return true;
  const life = lifecycleLabel(lifecycleState(c));
  const hay = [c.id, c.title, c.author, c.agent, c.branch, c.intent, c.state, life, ...(c.workItems ?? [])]
    .filter(Boolean)
    .join(' ')
    .toLowerCase();
  return words.every((w) => hay.includes(w));
}

/**
 * Order a list so that stacked children follow their parent (depth-first).
 * A child whose parent is not in the list stays at depth 0.
 */
export function stackOrder(list: ChangeSummary[]): RailItem[] {
  const ids = new Set(list.map((c) => c.id));
  const kids = new Map<string, ChangeSummary[]>();
  for (const c of list) {
    if (c.parent && ids.has(c.parent) && c.parent !== c.id) {
      const k = kids.get(c.parent) ?? [];
      k.push(c);
      kids.set(c.parent, k);
    }
  }
  const out: RailItem[] = [];
  const seen = new Set<string>();
  const walk = (c: ChangeSummary, depth: number) => {
    if (seen.has(c.id)) return;
    seen.add(c.id);
    out.push({ c, depth });
    for (const k of kids.get(c.id) ?? []) walk(k, depth + 1);
  };
  for (const c of list) {
    if (c.parent && ids.has(c.parent)) continue;
    walk(c, 0);
  }
  // Cycles or odd data: keep every change visible.
  for (const c of list) if (!seen.has(c.id)) walk(c, 0);
  return out;
}

/** Rail groups for a filter, sort and search query. Empty groups are left out. */
export function railGroups(
  list: ChangeSummary[],
  filter: RailFilter,
  sort: RailSort,
  query = '',
  recent: ChangeSummary[] = [],
): RailGroup[] {
  const sorted = list.filter((c) => matches(c, query)).sort(compare(sort));
  const merged = recent.filter((c) => matches(c, query) && !list.some((o) => o.id === c.id));
  const out: RailGroup[] = [];
  for (const g of GROUPS) {
    if (filter !== 'all' && g.key !== filter) continue;
    // A stacked child shows under its parent when both are in the group.
    out.push({ ...g, items: stackOrder(sorted.filter((c) => c.group === g.key)) });
    // My recent merges get their own group after my open changes (rollout
    // to watch), so "My changes" counts only open ones.
    if (g.key === 'mine' && merged.length) out.push({ key: 'merged', label: MERGED_LABEL, items: merged.map((c) => ({ c, depth: 0 })) });
  }
  return out.filter((g) => g.items.length > 0);
}

/** Label of the group of my recent merges. */
export const MERGED_LABEL = 'My recent merges';

export interface Hint {
  text: string;
  tone?: 'warn' | 'bad' | 'info' | 'ok';
  title?: string;
}

/** Second-line hints of a rail item: author, agent, stack, behind, dirty. */
export function railHints(c: ChangeSummary): Hint[] {
  const out: Hint[] = [];
  const who = c.agent && c.agent !== c.author ? `${c.author} + agent` : c.author;
  if (who) out.push({ text: who, title: c.agent ? `commits by ${c.agent}` : undefined });
  if (['merged', 'deployed', 'verified', 'closed'].includes(c.state)) return out;
  if (c.parent) out.push({ text: `on ${c.parent}`, title: `stacked on ${c.parent}` });
  if (c.behindTarget > 0) {
    const target = c.parent ? c.parent : c.target;
    out.push({ text: `behind ${target} by ${c.behindTarget}`, tone: 'warn' });
  }
  if (c.dirty) out.push({ text: 'uncommitted', tone: 'info', title: 'saved files not committed yet' });
  else if (c.local && !c.pushed && c.commits > 1) out.push({ text: 'not pushed' });
  else if (c.aheadRemote > 0) out.push({ text: `${c.aheadRemote} to push` });
  return out;
}

/** Counts per group (for the filter chips). */
export function groupCounts(list: ChangeSummary[]): Record<RailFilter, number> {
  const n: Record<RailFilter, number> = { all: list.length, mine: 0, others: 0, review: 0, bots: 0 };
  for (const c of list) n[c.group]++;
  return n;
}
