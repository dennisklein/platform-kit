// Checks the actions page (src/content/docs/reference/actions.mdx) against the
// actions that the API defines (src/data/generated/actions.json, written by
// metal/hack/docgen from the HostOperation types). The page is written by hand,
// in STE; this check makes sure that it cannot miss an action or a field.
//
// Fails when
//   - the number in the description is not the number of actions,
//   - an action is not in the `anchors.act` frontmatter, the Summary table (with its
//     key), or as a `### <Name>` section,
//   - the page names an action that the API does not have,
//   - a field of an action is not in its section (as `field`), or a field row of a
//     section's table names a field that the action does not have.
// Usage: node scripts/check-actions.mjs
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parse } from 'yaml';
import { root } from './root.mjs';

const pageFile = 'src/content/docs/reference/actions.mdx';
const page = readFileSync(join(root, pageFile), 'utf8');
const { actions } = JSON.parse(readFileSync(join(root, 'src/data/generated/actions.json'), 'utf8'));
const fm = parse(/^---\r?\n([\s\S]*?)\r?\n---/.exec(page)[1]);
const errors = [];
const err = (m) => errors.push(m);

const names = new Set(actions.map((a) => a.goName));
const count = /\bThe (\d+) actions\b/.exec(fm.description ?? '')?.[1];
if (Number(count) !== actions.length) err(`the description says "${count ?? 'no number'}" actions; the API has ${actions.length}`);

const anchors = new Set(fm.anchors?.act ?? []);
const summary = new Map([...page.matchAll(/^\| \[\]\(act:(\w+)\) \| `(\w+)` \|/gm)].map((m) => [m[1], m[2]]));
const sections = new Map();
const parts = page.split(/^### /m).slice(1);
for (const part of parts) {
  const name = part.slice(0, part.indexOf('\n')).trim();
  sections.set(name, part.split(/^## /m)[0]);
}

for (const a of actions) {
  if (!anchors.has(a.goName)) err(`${a.goName}: not in the anchors.act frontmatter`);
  if (!summary.has(a.goName)) err(`${a.goName}: no row in the Summary table`);
  else if (summary.get(a.goName) !== a.key) err(`${a.goName}: the Summary table has the key \`${summary.get(a.goName)}\`, the API \`${a.key}\``);
  const body = sections.get(a.goName);
  if (!body) {
    err(`${a.goName}: no "### ${a.goName}" section`);
    continue;
  }
  const fields = new Set(a.fields.map((f) => f.name));
  for (const f of fields) if (!body.includes(`\`${f}\``)) err(`${a.goName}: the section does not name the field \`${f}\``);
  for (const m of body.matchAll(/^\| `([\w.]+)` \|/gm)) {
    if (!fields.has(m[1])) err(`${a.goName}: the table has a row for \`${m[1]}\`, which the action does not have`);
  }
}
for (const n of new Set([...anchors, ...summary.keys(), ...sections.keys()])) {
  if (!names.has(n)) err(`${n}: on the page, but the API has no such action`);
}

for (const e of errors) console.error(`check-actions: ${pageFile}: ${e}`);
console.log(`check-actions: ${actions.length} actions checked, ${errors.length} errors`);
process.exit(errors.length ? 1 : 0);
