// The category link registry: every id that `[](<scheme>:<id>)` can name.
//
// Sources, in order of precedence:
//   1. the `xref` frontmatter of the pages in src/content/docs (a page owns its id),
//   2. the `anchors` frontmatter of a page: more ids, each at an anchor of that page
//      (`act: [PowerCycle]` on the actions page is `#powercycle` there),
//   3. src/data/xref-planned.yaml (pages that do not exist yet; optional),
//   4. the glossary (`term:<id>`).
// An id that none of them knows is an error: scripts/xref.mjs stops the build.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import { parse } from 'yaml';
import { byId } from '../src/data/categories.mjs';
import { loadGlossary } from './glossary-data.mjs';
import { root } from './root.mjs';

const contentDir = join(root, 'src/content/docs');
const plannedFile = join(root, 'src/data/xref-planned.yaml');

/** Where a planned id goes when the entry gives no path. */
const pathRule = {
  kind: (id) => `reference/kinds/${id.toLowerCase()}/`,
  ctrl: (id) => `reference/controllers/${id}/`,
  act: (id) => `reference/actions/#${id.toLowerCase()}`,
  cli: (id) => `reference/cli/${id}/`,
  lab: (id) => `reference/lab/tasks/#${id}`,
  ref: (id) => `reference/${id}/`,
  concept: (id) => `concepts/${id}/`,
  flow: (id) => `workflows/${id}/`,
  howto: (id) => `operate/${id}/`,
  rb: (id) => `operate/runbooks/${id}/`,
  tut: (id) => `learn/${id}/`,
};

function walk(dir) {
  if (!existsSync(dir)) return [];
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) return walk(p);
    return /\.mdx?$/.test(name) ? [p] : [];
  });
}

function frontmatter(file) {
  const m = /^---\r?\n([\s\S]*?)\r?\n---/.exec(readFileSync(file, 'utf8'));
  return m ? parse(m[1]) ?? {} : {};
}

/** The URL path of a content file, without the base: `reference/kinds/host/`. */
export function slugOf(file) {
  const rel = relative(contentDir, file).split(sep).join('/').replace(/\.mdx?$/, '');
  if (rel === 'index') return '';
  return `${rel.replace(/\/index$/, '').toLowerCase()}/`;
}

/** The anchor of a heading, as Starlight makes it: "Policy loop" is "policy-loop". */
export const slugify = (s) => String(s).toLowerCase().trim().replace(/[^\p{L}\p{N}\s_-]/gu, '').replace(/\s/g, '-');

/** The ids of a page's `anchors` frontmatter, as [cat, id, anchor, title]. */
export function anchorsOf(page) {
  const out = [];
  for (const [cat, v] of Object.entries(page.data.anchors ?? {})) {
    if (!v) throw new Error(`xref: ${page.file} has an empty anchors.${cat}`);
    const entries = Array.isArray(v) ? v.map((id) => [id, undefined]) : Object.entries(v);
    for (const [id, a] of entries) {
      const e = typeof a === 'string' ? { anchor: a } : a ?? {};
      out.push([cat, String(id), e.anchor ?? slugify(id), e.title]);
    }
  }
  return out;
}

/** All pages: slug, frontmatter and file. */
export function loadPages() {
  return walk(contentDir).map((file) => ({ file, slug: slugOf(file), data: frontmatter(file) }));
}

let cache;

/**
 * @param {string} base the site base, for example "/platform-kit"
 * @returns {Map<string, {cat: string, id: string, href: string, title: string, description?: string, planned: boolean, file?: string}>}
 */
export function loadRegistry(base) {
  if (cache) return cache;
  const b = base.replace(/\/$/, '');
  const reg = new Map();
  for (const page of loadPages()) {
    const x = page.data.xref;
    if (!x) continue;
    if (!byId.has(x.cat)) throw new Error(`xref: ${page.file} has the unknown category "${x.cat}"`);
    const key = `${x.cat}:${x.id}`;
    if (reg.has(key)) throw new Error(`xref: "${key}" is in ${reg.get(key).file} and in ${page.file}`);
    reg.set(key, {
      cat: x.cat, id: String(x.id), href: `${b}/${page.slug}`, title: page.data.title,
      description: page.data.description, planned: false, file: page.file,
    });
  }
  for (const page of loadPages()) {
    for (const [cat, id, anchor, title] of anchorsOf(page)) {
      if (!byId.has(cat)) throw new Error(`xref: ${page.file} has anchors of the unknown category "${cat}"`);
      const key = `${cat}:${id}`;
      if (reg.has(key)) throw new Error(`xref: "${key}" is in ${reg.get(key).file} and in the anchors of ${page.file}`);
      reg.set(key, {
        cat, id, href: `${b}/${page.slug}#${anchor}`, title: title ?? id,
        description: page.data.description, planned: false, file: page.file, anchor: true,
      });
    }
  }
  if (existsSync(plannedFile)) {
    const planned = parse(readFileSync(plannedFile, 'utf8')) ?? {};
    for (const [cat, ids] of Object.entries(planned)) {
      if (!pathRule[cat]) throw new Error(`xref: ${plannedFile} has the unknown category "${cat}"`);
      // An empty category ("kind:" with no ids) parses as null. Skip it.
      if (ids === null || ids === undefined) continue;
      if (typeof ids !== 'object') throw new Error(`xref: ${plannedFile}: "${cat}" must map ids to titles`);
      for (const [id, v] of Object.entries(ids)) {
        const key = `${cat}:${id}`;
        if (reg.has(key)) continue;
        const e = typeof v === 'string' ? { title: v } : v ?? { title: id };
        const path = e.path ?? (id === 'index' ? pathRule[cat]('x').replace(/x\/$|#x$/, '') : pathRule[cat](id));
        reg.set(key, {
          cat, id, href: `${b}/${path}`, title: e.title,
          description: e.description, planned: true,
        });
      }
    }
  }
  const glossaryPage = loadPages().some((p) => p.slug === 'reference/glossary/');
  for (const t of loadGlossary().terms) {
    reg.set(`term:${t.id}`, {
      cat: 'term', id: t.id, href: `${b}/reference/glossary/#${t.id}`, title: t.term,
      description: t.def, planned: !glossaryPage,
    });
  }
  cache = reg;
  return reg;
}

/** The text of an empty category link: `[](cli:approve)` shows "pk-metal approve". */
export function canonicalText(entry) {
  const cat = byId.get(entry.cat);
  if (entry.id === 'index' || entry.id === 'global-flags') return entry.title;
  if (entry.cat === 'lab' && entry.id.startsWith('page/')) return entry.title;
  if (cat.prefix) return cat.prefix + entry.id;
  if (entry.cat === 'kind' || entry.cat === 'act') return entry.id;
  return entry.title;
}
