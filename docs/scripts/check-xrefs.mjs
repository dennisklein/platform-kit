// Checks the category links in the built site (mise run docs:links).
//
// Fails when
//   - a category link points to a page or an anchor that is not in dist/
//     (links to planned pages, data-planned, are counted but not checked),
//   - a link into reference/ has no category (write `[](kind:Host)`, not a path),
//   - a reference page has no `xref` frontmatter,
//   - an id of a page's `anchors` frontmatter has no anchor on that page,
//   - a page with an id has no category link from another page (an orphan),
//   - a back-link rule fails: a kind page links no controller, a controller page links
//     no kind, a command page links no how-to, runbook or tutorial.
// Usage: node scripts/check-xrefs.mjs [dist]
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { base } from './site.mjs';
import { anchorsOf, loadPages } from './registry.mjs';
import { root } from './root.mjs';

const dist = process.argv[2] ?? join(root, 'dist');
if (!existsSync(dist)) {
  console.error(`check-xrefs: ${dist} does not exist. Run mise run docs:build first.`);
  process.exit(1);
}

const walk = (d) => readdirSync(d).flatMap((n) => {
  const p = join(d, n);
  return statSync(p).isDirectory() ? walk(p) : n.endsWith('.html') ? [p] : [];
});

const ids = new Map();
const pages = walk(dist).map((file) => {
  const html = readFileSync(file, 'utf8');
  const path = `${base}/${file.slice(dist.length + 1).replace(/index\.html$/, '')}`;
  ids.set(path, new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])));
  return { file, path, html };
});

const errors = [];
let planned = 0;
let checked = 0;
/** Category links in page content: target path -> pages that link it; page -> categories it links. */
const inbound = new Map();
const outCats = new Map();
for (const { path, html } of pages) {
  const content = (html.split('class="sl-markdown-content"')[1] ?? '').split('<footer')[0];
  const cats = new Set();
  for (const m of content.matchAll(/<a\s([^>]*)>/g)) {
    const href = /href="([^"#]*)/.exec(m[1])?.[1];
    const cat = /data-cat="([^"]*)"/.exec(m[1])?.[1];
    if (!href || !cat) continue;
    cats.add(cat);
    if (href !== path) {
      if (!inbound.has(href)) inbound.set(href, new Set());
      inbound.get(href).add(path);
    }
  }
  outCats.set(path, cats);
}
for (const { path, html } of pages) {
  for (const m of html.matchAll(/<a\s([^>]*)>/g)) {
    const attrs = m[1];
    const href = /href="([^"]*)"/.exec(attrs)?.[1];
    if (!href || !href.startsWith(`${base}/`)) continue;
    const cat = /data-cat="([^"]*)"/.exec(attrs)?.[1];
    const inMarkdown = /class="[^"]*\b(x|gl)\b/.test(attrs);
    if (!inMarkdown) continue;
    if (/data-planned/.test(attrs)) {
      planned++;
      continue;
    }
    checked++;
    const [target, hash] = href.split('#');
    const t = ids.get(target);
    if (!t) errors.push(`${path}: ${href}: no page`);
    else if (hash && !t.has(decodeURIComponent(hash))) errors.push(`${path}: ${href}: no anchor "${hash}"`);
  }
  // Plain links into the reference, inside the page content.
  const content = (html.split('class="sl-markdown-content"')[1] ?? '').split('<footer')[0];
  for (const m of content.matchAll(/<a\s([^>]*)>/g)) {
    const href = /href="([^"]*)"/.exec(m[1])?.[1] ?? '';
    if (href.startsWith(`${base}/reference/`) && !/data-cat=|class="[^"]*\bgl\b/.test(m[1]) && !/class="[^"]*(f-anchor|box-g|pk-)/.test(m[1])) {
      errors.push(`${path}: ${href}: a link into the reference needs a category, for example [](kind:Host)`);
    }
  }
}

const backlinks = {
  kind: { needs: ['ctrl'], what: 'a kind page must link its controller' },
  controller: { needs: ['kind'], what: 'a controller page must link its kinds' },
  cli: { needs: ['howto', 'rb', 'tut'], what: 'a command page must link a how-to, a runbook or a tutorial' },
};
const orphans = [];
for (const p of loadPages()) {
  if (p.slug.startsWith('reference/') && !p.data.xref) errors.push(`${p.file}: a reference page needs xref frontmatter`);
  const path = `${base}/${p.slug}`;
  const t = ids.get(path);
  for (const [cat, id, anchor] of anchorsOf(p)) {
    if (t && !t.has(anchor)) errors.push(`${p.file}: anchors ${cat}:${id}: no anchor "${anchor}" on the page`);
  }
  if (!p.data.xref || p.data.type === 'landing') continue;
  if (!inbound.get(path)?.size) orphans.push(path);
  const rule = backlinks[p.data.type];
  const isIndex = p.data.xref.id === 'index' || p.data.xref.id === 'global-flags';
  if (rule && !isIndex && !rule.needs.some((c) => outCats.get(path)?.has(c))) errors.push(`${path}: ${rule.what}`);
}
for (const o of orphans) errors.push(`${o}: no other page links here (orphan); link it with its category id`);

for (const e of errors) console.error(`check-xrefs: ${e}`);
console.log(`check-xrefs: ${checked} links checked, ${planned} links to planned pages, ${errors.length} errors`);
process.exit(errors.length ? 1 : 0);
