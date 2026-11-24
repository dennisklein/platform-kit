// Loads the glossary: src/data/glossary.yaml, or the sample file while that does not exist.
import { existsSync, readFileSync } from 'node:fs';
import { parse } from 'yaml';

import { root } from './root.mjs';

const dataDir = `${root}/src/data/`;
let cache;

/** @returns {{ domains: Record<string, {name: string, icon: string}>, terms: any[], file: string }} */
export function loadGlossary() {
  if (cache) return cache;
  const file = ['glossary.yaml', 'glossary-sample.yaml'].map((f) => dataDir + f).find((f) => existsSync(f));
  if (!file) throw new Error('glossary: no src/data/glossary.yaml');
  const data = parse(readFileSync(file, 'utf8'));
  const seen = new Map();
  for (const t of data.terms) {
    if (!data.domains[t.domain]) throw new Error(`glossary: term "${t.id}" has the unknown domain "${t.domain}"`);
    for (const f of t.forms ?? [t.term]) {
      if (seen.has(f)) throw new Error(`glossary: the form "${f}" is in "${seen.get(f)}" and in "${t.id}"`);
      seen.set(f, t.id);
    }
  }
  cache = { domains: data.domains, terms: data.terms, file };
  return cache;
}
