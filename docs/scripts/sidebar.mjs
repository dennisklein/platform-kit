// The sidebar: the site's top-level groups, in reading order. Each group lists the
// pages in its folder, and the planned pages from the optional file
// src/data/xref-planned.yaml (shown with a "planned" mark) when that file exists. src/lib/tabs.ts maps the groups to the
// tabs of the header.
import { loadPages, loadRegistry } from './registry.mjs';

/**
 * Top-level groups. `dir` is the folder under src/content/docs; `groups` nest one level.
 * `ordered`: the pages are in reading order, so each page sets a unique `sidebar.order`
 * (the build fails otherwise). Other groups sort by title.
 */
export const layout = [
  { label: 'Start', dir: 'start', ordered: true },
  { label: 'Tutorials', dir: 'learn', ordered: true },
  { label: 'Concepts', dir: 'concepts', ordered: true },
  { label: 'Workflows', dir: 'workflows', ordered: true },
  {
    label: 'Operate',
    groups: [
      { label: 'Day 1', dir: 'operate/day-1', ordered: true },
      { label: 'Day 2', dir: 'operate/day-2', ordered: true },
      { label: 'Runbooks', dir: 'operate/runbooks' },
    ],
  },
  {
    label: 'Reference',
    groups: [
      { label: 'Kinds', dir: 'reference/kinds' },
      { label: 'Controllers', dir: 'reference/controllers' },
      { label: 'Commands', dir: 'reference/cli' },
      { label: 'Lab', dir: 'reference/lab', ordered: true },
      { label: 'Security', dir: 'reference/security' },
      { label: 'Catalogs', dir: 'reference', flat: true },
    ],
  },
  { label: 'Project', dir: 'project' },
];

/** Pages that are anchors targets of several ids, by path. */
const anchorPages = { 'reference/actions/': 'Actions', 'reference/lab/tasks/': 'Lab tasks', 'reference/glossary/': 'Glossary' };

/**
 * Is the page at `slug` in the folder `dir`? A flat group takes only the pages
 * directly in its folder: not the pages of a sub-folder, and not the index page of
 * a sub-folder either (`reference/kinds/` belongs to Kinds, not to Catalogs).
 */
function inFolder(slug, dir, flat) {
  if (!slug.startsWith(`${dir}/`)) return false;
  if (!flat) return true;
  return !slug.slice(dir.length + 1, -1).includes('/') && !folderIndexes().has(slug);
}

let indexes;
/** The slugs of the index pages of folders: each slug that is a prefix of another page's slug. */
function folderIndexes() {
  if (indexes) return indexes;
  const slugs = loadPages().map((p) => p.slug);
  indexes = new Set(slugs.filter((s) => s && slugs.some((o) => o !== s && o.startsWith(s))));
  return indexes;
}

function items(dir, flat, base) {
  const inDir = (slug) => inFolder(slug, dir, flat);
  const pages = loadPages()
    .filter((p) => inDir(p.slug) && p.data.type !== 'landing')
    .map((p) => ({
      // The index page of a folder is its "Overview", unless the page names itself.
      label: p.data.sidebar?.label ?? (p.slug === `${dir}/` ? 'Overview' : p.data.title),
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
    const ok = dirs.some((d) => inFolder(p.slug, d.dir, d.flat));
    if (!ok) throw new Error(`sidebar: ${p.file} is in a folder that no sidebar group shows. Add the folder to scripts/sidebar.mjs.`);
  }
}

/**
 * Pages that break the order rule today, as "file: reason". The check fails when an
 * entry here is no longer needed, so the list only shrinks. Remove an entry in the
 * same change that fixes the page's frontmatter.
 */
const orderExceptions = new Set([]);

/** In an ordered group, each page (except the folder index) has its own sidebar.order. */
function checkOrder() {
  const rel = (file) => file.split('/src/content/docs/')[1];
  const found = [];
  for (const d of layout.flatMap((g) => g.groups ?? [g]).filter((d) => d.ordered)) {
    const seen = new Map();
    const pages = loadPages()
      .filter((p) => inFolder(p.slug, d.dir, d.flat) && p.slug !== `${d.dir}/` && p.data.type !== 'landing')
      .sort((a, b) => a.file.localeCompare(b.file));
    for (const p of pages) {
      const order = p.data.sidebar?.order;
      if (typeof order !== 'number') found.push(`${rel(p.file)}: no order`);
      else if (seen.has(order)) found.push(`${rel(p.file)}: order ${order} also in ${rel(seen.get(order))}`);
      else seen.set(order, p.file);
    }
  }
  const errors = found.filter((f) => !orderExceptions.has(f));
  const stale = [...orderExceptions].filter((e) => !found.includes(e));
  if (errors.length || stale.length) {
    throw new Error(
      'sidebar: the pages of an ordered group (scripts/sidebar.mjs, `ordered: true`) each need a unique `sidebar.order`.\n' +
        errors.map((e) => `  ${e}\n`).join('') +
        stale.map((e) => `  fixed, remove from orderExceptions: ${e}\n`).join(''),
    );
  }
}

/** The Starlight `sidebar` option. */
export function sidebar({ base }) {
  checkFolders();
  checkOrder();
  return layout
    .map((g) => {
      const entries = g.groups
        ? g.groups.map((s) => ({ label: s.label, collapsed: false, items: items(s.dir, s.flat, base) })).filter((s) => s.items.length)
        : items(g.dir, false, base);
      return { label: g.label, items: entries };
    })
    .filter((g) => g.items.length);
}
