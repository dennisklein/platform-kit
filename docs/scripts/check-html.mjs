// Checks the built pages for accessibility and HTML faults that a diagram or a
// component can bring in (mise run docs:links).
//
// Fails when a page has
//   - a link inside an <svg role="img">: the role hides the link from screen readers
//     (give the SVG the role "group", as Figure.astro and Explainer.astro do),
//   - an id two times (for example a marker that each diagram defines again; the
//     shared markers are in src/components/diagram/SvgDefs.astro).
// Usage: node scripts/check-html.mjs [dist]
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { root } from './root.mjs';

const dist = process.argv[2] ?? join(root, 'dist');
if (!existsSync(dist)) {
  console.error(`check-html: ${dist} does not exist. Run mise run docs:build first.`);
  process.exit(1);
}

const walk = (d) => readdirSync(d).flatMap((n) => {
  const p = join(d, n);
  return statSync(p).isDirectory() ? walk(p) : n.endsWith('.html') ? [p] : [];
});

/** The <svg> elements with role="img", each with its whole body (nested SVGs included). */
function imgSvgs(html) {
  const out = [];
  const open = /<svg\b[^>]*>/g;
  for (let m = open.exec(html); m; m = open.exec(html)) {
    if (!/\srole="img"/.test(m[0])) continue;
    const tags = /<svg\b|<\/svg>/g;
    tags.lastIndex = m.index;
    let depth = 0;
    for (let t = tags.exec(html); t; t = tags.exec(html)) {
      depth += t[0] === '</svg>' ? -1 : 1;
      if (depth === 0) {
        out.push(html.slice(m.index, t.index));
        break;
      }
    }
  }
  return out;
}

const errors = [];
let pages = 0;
for (const file of walk(dist)) {
  const html = readFileSync(file, 'utf8');
  const page = file.slice(dist.length + 1);
  pages++;
  for (const svg of imgSvgs(html)) {
    const links = svg.match(/<a\s/g)?.length ?? 0;
    if (links) errors.push(`${page}: ${links} link(s) inside <svg role="img">; use role="group"`);
  }
  // Ids outside <template> and <script>: those are not in the document tree.
  const tree = html.replace(/<(template|script)\b[\s\S]*?<\/\1>/g, '');
  const seen = new Map();
  for (const m of tree.matchAll(/<[a-zA-Z][^>]*?\sid="([^"]+)"/g)) seen.set(m[1], (seen.get(m[1]) ?? 0) + 1);
  for (const [id, n] of seen) if (n > 1) errors.push(`${page}: the id "${id}" is on ${n} elements`);
}

for (const e of errors) console.error(`check-html: ${e}`);
console.log(`check-html: ${pages} pages checked, ${errors.length} errors`);
process.exit(errors.length ? 1 : 0);
