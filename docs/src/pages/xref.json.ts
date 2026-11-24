// Data for the hover cards: every category link id and every glossary term.
import type { APIRoute } from 'astro';
import { loadRegistry } from '../../scripts/registry.mjs';
import { loadGlossary } from '../../scripts/glossary-data.mjs';
import { categories } from '../data/categories.mjs';

export const GET: APIRoute = () => {
  const xrefs: Record<string, unknown> = {};
  for (const [key, e] of loadRegistry(import.meta.env.BASE_URL)) {
    // A link to an alias carries the current id in data-xref, so the card needs no alias.
    if (e.cat === 'term' || e.alias) continue;
    xrefs[key] = { cat: e.cat, title: e.title, description: e.description, planned: e.planned };
  }
  const { domains, terms } = loadGlossary();
  return new Response(
    JSON.stringify({
      categories: Object.fromEntries(categories.map((c) => [c.id, c.label])),
      xrefs,
      domains,
      terms: Object.fromEntries(terms.map((t) => [t.id, { term: t.term, abbr: t.abbr, kind: t.kind, domain: t.domain, def: t.def }])),
    }),
    { headers: { 'content-type': 'application/json' } },
  );
};
