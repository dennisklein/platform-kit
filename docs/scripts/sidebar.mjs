// The sidebar: the site's top-level groups, in reading order. Each group lists the
// pages in its folder, and the planned pages from src/data/xref-planned.yaml (shown
// with a "planned" mark) while that file exists. src/lib/tabs.ts maps the groups to the
// tabs of the header.
import { loadPages, loadRegistry } from './registry.mjs';

/** Top-level groups. `dir` is the folder under src/content/docs; `groups` nest one level. */
export const layout = [
  { label: 'Start', dir: 'start' },
  { label: 'Learn', dir: 'learn' },
  { label: 'Concepts', dir: 'concepts' },
  { label: 'Workflows', dir: 'workflows' },
  {
    label: 'Operate',
    groups: [
      { label: 'Day 1', dir: 'operate/day-1' },
      { label: 'Day 2', dir: 'operate/day-2' },
      { label: 'Runbooks', dir: 'operate/runbooks' },
    ],
  },
  {
    label: 'Reference',
    groups: [
      { label: 'Kinds', dir: 'reference/kinds' },
      { label: 'Controllers', dir: 'reference/controllers' },
      { label: 'Commands', dir: 'reference/cli' },
      { label: 'Lab', dir: 'reference/lab' },
      { label: 'Security', dir: 'reference/security' },
      { label: 'Catalogs', dir: 'reference', flat: true },
    ],
  },
  { label: 'Project', dir: 'project' },
];

/** Pages that are anchors targets of several ids, by path. */
const anchorPages = { 'reference/actions/': 'Actions', 'reference/lab/tasks/': 'Lab tasks', 'reference/glossary/': 'Glossary' };

function items(dir, flat, base) {
  const inDir = (slug) => {
    if (!slug.startsWith(`${dir}/`)) return false;
    return !flat || !slug.slice(dir.length + 1, -1).includes('/');
  };
  const pages = loadPages()
    .filter((p) => inDir(p.slug) && p.data.type !== 'landing')
    .map((p) => ({
      label: p.data.sidebar?.label ?? p.data.title,
      link: `/${p.slug}`,
      order: p.data.sidebar?.order ?? (p.slug.endsWith(`${dir}/`) ? -1 : 100),
    }));
  const have = new Set(pages.map((p) => p.link));
  const b = base.replace(/\/$/, '');
  for (const e of loadRegistry(base).values()) {
    if (!e.planned || e.cat === 'term') continue;
    const path = e.href.slice(b.length + 1).split('#')[0];
    if (!inDir(path) || have.has(`/${path}`)) continue;
    have.add(`/${path}`);
    pages.push({ label: anchorPages[path] ?? e.title, link: `/${path}`, order: path === `${dir}/` ? -1 : 200, planned: true });
  }
  for (const [path, label] of Object.entries(anchorPages)) {
    if (inDir(path) && !have.has(`/${path}`) && flat) pages.push({ label, link: `/${path}`, order: 200, planned: true });
  }
  return pages
    .sort((x, y) => x.order - y.order || x.label.localeCompare(y.label))
    .map(({ label, link, planned }) => ({ label, link, ...(planned ? { attrs: { 'data-planned': '' } } : {}) }));
}

/** Every page must be in one group, or it is in no tab of the header. */
function checkFolders() {
  const dirs = layout.flatMap((g) => (g.groups ?? [g]).map((s) => s));
  for (const p of loadPages()) {
    if (p.data.type === 'landing' || p.slug === '404/') continue;
    const ok = dirs.some((d) => p.slug.startsWith(`${d.dir}/`) && (!d.flat || !p.slug.slice(d.dir.length + 1, -1).includes('/')));
    if (!ok) throw new Error(`sidebar: ${p.file} is in a folder that no sidebar group shows. Add the folder to scripts/sidebar.mjs.`);
  }
}

/** The Starlight `sidebar` option. */
export function sidebar({ base }) {
  checkFolders();
  return layout
    .map((g) => {
      const entries = g.groups
        ? g.groups.map((s) => ({ label: s.label, collapsed: false, items: items(s.dir, s.flat, base) })).filter((s) => s.items.length)
        : items(g.dir, false, base);
      return { label: g.label, items: entries };
    })
    .filter((g) => g.items.length);
}
