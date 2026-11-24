// The top tabs. Each tab shows the sidebar groups that it owns, and opens the
// overview page of its layer.
export const tabs = [
  { label: 'Learn', groups: ['Start', 'Tutorials', 'Concepts', 'Workflows'], href: 'start/overview/' },
  { label: 'Operate', groups: ['Operate'], href: 'operate/day-1/' },
  { label: 'Reference', groups: ['Reference', 'Project'], href: 'reference/kinds/' },
  { label: 'Lab', groups: ['Lab'], href: 'reference/lab/' },
];

type Entry = { type: 'link' | 'group'; label: string; isCurrent?: boolean; entries?: Entry[] };

const hasCurrent = (e: Entry): boolean => (e.type === 'link' ? !!e.isCurrent : (e.entries ?? []).some(hasCurrent));

/** The Lab tab takes the Lab group out of Reference. */
function split(sidebar: Entry[]): Entry[] {
  const out: Entry[] = [];
  for (const g of sidebar) {
    if (g.label === 'Reference' && g.entries) {
      const lab = g.entries.find((e) => e.label === 'Lab');
      out.push({ ...g, entries: g.entries.filter((e) => e !== lab) });
      if (lab) out.push({ ...lab, label: 'Lab' });
    } else out.push(g);
  }
  return out;
}

/** The index of the current tab, and the sidebar groups of that tab. */
export function forRoute(sidebar: Entry[]) {
  const groups = split(sidebar);
  const cur = groups.find(hasCurrent);
  const index = Math.max(0, tabs.findIndex((t) => cur && t.groups.includes(cur.label)));
  return { index, groups: groups.filter((g) => tabs[index].groups.includes(g.label)) };
}
