// Writes the prose of the generated reference data (src/data/generated: field and
// action descriptions from Go doc comments, command help, condition and phase
// notes) to Markdown files, so that Vale can check it with the STE style
// (mise run docs:lint:generated). Each text is under a heading that names its
// source, so a finding says where to fix it: the Go comment or the help text
// that metal/hack/docgen read.
// Usage: node scripts/generated-text.mjs <out-dir>
import { mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { root } from './root.mjs';

const out = process.argv[2];
if (!out) {
  console.error('usage: node scripts/generated-text.mjs <out-dir>');
  process.exit(2);
}
const dir = join(root, 'src/data/generated');
const prose = new Set(['description', 'typeDescription', 'summary', 'context', 'meaning', 'usage']);
/** The keys that name an entry, in order of preference, for the heading. */
const names = ['path', 'goName', 'name', 'key', 'value', 'code', 'kind'];

const walk = (d) => readdirSync(d).flatMap((n) => {
  const p = join(d, n);
  return statSync(p).isDirectory() ? walk(p) : n.endsWith('.json') ? [p] : [];
});

/** [heading, text] for each prose string in a JSON value. */
function collect(v, where, acc) {
  if (Array.isArray(v)) v.forEach((x) => collect(x, where, acc));
  else if (v && typeof v === 'object') {
    const id = names.map((k) => v[k]).find((x) => typeof x === 'string' || typeof x === 'number');
    const here = id === undefined ? where : `${where} ${id}`;
    for (const [k, x] of Object.entries(v)) {
      if (prose.has(k) && typeof x === 'string' && x.trim()) acc.push([`${here} (${k})`, x]);
      else collect(x, here, acc);
    }
  }
  return acc;
}

mkdirSync(out, { recursive: true });
let texts = 0;
for (const file of walk(dir)) {
  const rel = relative(dir, file);
  const entries = collect(JSON.parse(readFileSync(file, 'utf8')), '', []);
  if (!entries.length) continue;
  texts += entries.length;
  // Headings as text, so a finding's line leads to the entry; Vale does not lint headings for length.
  const md = entries.map(([h, t]) => `## ${h.trim().replace(/[`*_]/g, '')}\n\n${t.trim()}\n`).join('\n');
  writeFileSync(join(out, `${rel.replace(/[/\\]/g, '--').replace(/\.json$/, '')}.md`), `# ${rel}\n\n${md}`);
}
console.log(`generated-text: ${texts} texts from ${dir} in ${out}`);
