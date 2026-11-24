// Sätteri hast plugin: links the first use of each glossary term in each section.
//
// A section starts at each h2. A term with `scope: page` (a word that most pages use
// often, such as operator or node) links only once on each page. The plugin skips headings, code, links, keyboard keys,
// SVG and every element with the class "no-gl". A term with `abbr` gets an <abbr>, so
// the expansion is there without JavaScript and for screen readers.
// src/components/TermCard.astro shows the hover card.
import { loadGlossary } from './glossary-data.mjs';
import { loadPages } from './registry.mjs';

const skipTags = new Set(['a', 'code', 'pre', 'kbd', 'svg', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'script', 'style', 'abbr', 'figure', 'th']);
const escape = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

export default function glossary({ base }) {
  const { terms } = loadGlossary();
  const b = base.replace(/\/$/, '');
  const forms = [];
  for (const t of terms) for (const f of t.forms ?? [t.term]) forms.push({ form: f, term: t });
  forms.sort((x, y) => y.form.length - x.form.length);
  const byForm = new Map(forms.map((f) => [f.form, f.term]));
  const re = new RegExp(`(?<![\\w-])(${forms.map((f) => escape(f.form)).join('|')})(?![\\w-])`, 'g');
  const page = loadPages().some((p) => p.slug === 'reference/glossary/');
  const scoped = new Set(terms.filter((t) => t.scope === 'page').map((t) => t.id));

  const link = (t, text) => {
    const inner = { type: 'text', value: text };
    return {
      type: 'element',
      tagName: 'a',
      properties: {
        href: `${b}/reference/glossary/#${t.id}`,
        className: ['gl'],
        dataGl: t.id,
        dataDomain: t.domain,
        ...(page ? {} : { dataPlanned: '' }),
      },
      children: t.abbr ? [{ type: 'element', tagName: 'abbr', properties: { title: t.abbr }, children: [inner] }] : [inner],
    };
  };

  // Guards (glossary.yaml `guards`): regular expressions on the text before the form
  // (only_after, not_after; anchor them with $) and after it (not_before; anchor with ^).
  const guard = (t, before, after) => {
    const g = t.guards ?? {};
    return (!g.only_after || new RegExp(g.only_after).test(before)) &&
      (!g.not_after || !new RegExp(g.not_after).test(before)) &&
      (!g.not_before || !new RegExp(g.not_before).test(after));
  };

  return {
    name: 'pk-glossary',
    before(root, ctx) {
      let used = new Set();
      // Page-scoped terms stay used across sections.
      const keep = () => new Set([...used].filter((id) => scoped.has(id)));
      const visitText = (node) => {
        const value = node.value;
        const out = [];
        let last = 0;
        for (const m of value.matchAll(re)) {
          const t = byForm.get(m[1]);
          if (used.has(t.id) || !guard(t, value.slice(0, m.index), value.slice(m.index + m[1].length))) continue;
          used.add(t.id);
          if (m.index > last) out.push({ type: 'text', value: value.slice(last, m.index) });
          out.push(link(t, m[1]));
          last = m.index + m[1].length;
        }
        if (!out.length) return;
        if (last < value.length) out.push({ type: 'text', value: value.slice(last) });
        ctx.replaceNode(node, out);
      };
      const walk = (node) => {
        for (const c of node.children ?? []) {
          if (c.type === 'text') visitText(c);
          else if (c.type === 'element') {
            if (c.tagName === 'h2') used = keep();
            const cls = [].concat(c.properties?.className ?? []);
            if (skipTags.has(c.tagName) || cls.includes('no-gl')) continue;
            walk(c);
          } else if (c.type === 'mdxJsxFlowElement' || c.type === 'mdxJsxTextElement') {
            walk(c);
          }
        }
      };
      walk(root);
    },
  };
}
