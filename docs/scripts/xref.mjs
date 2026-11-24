// Sätteri hast plugin: category links.
//
// `[](kind:Host)`, `[text](cli:approve#--yes)` and the other schemes in
// src/data/categories.mjs become links to the page that owns the id (see
// scripts/registry.mjs). The link gets `class="x"` and `data-cat`, so CSS gives it the
// color and the icon of its category. An empty link gets the canonical text. An
// unknown id stops the build with the file and the id in the message.
// Links to https:// get `data-cat="ext"`, links to /repo/<path> get `data-cat="src"`.
import { fileURLToPath } from 'node:url';
import { byScheme } from '../src/data/categories.mjs';
import { canonicalText, loadRegistry } from './registry.mjs';

const schemeRe = /^([a-z]+):([^#]+)(?:#(.*))?$/;

/** The text of a hast node and its children. */
function textOf(node) {
  if (node.type === 'text') return node.value;
  return (node.children ?? []).map(textOf).join('');
}

function where(ctx) {
  return ctx.fileURL ? fileURLToPath(ctx.fileURL) : 'a page';
}

export default function xref({ base }) {
  const reg = loadRegistry(base);
  return {
    name: 'pk-xref',
    element: {
      filter: ['a'],
      visit(node, ctx) {
        const href = node.properties?.href;
        if (typeof href !== 'string') return;
        const classes = [].concat(node.properties.className ?? []);
        if (classes.includes('gl') || classes.includes('x')) return;
        let cat;
        if (/^https?:\/\//.test(href)) cat = 'ext';
        else if (href.startsWith('/repo/')) cat = 'src';
        if (cat) {
          ctx.setProperty(node, 'className', [...classes, 'x']);
          ctx.setProperty(node, 'dataCat', cat);
          if (!node.children.length) ctx.appendChild(node, { type: 'text', value: href.replace(/^\/repo\/|^https?:\/\//, '') });
          return;
        }
        const m = schemeRe.exec(href);
        if (!m || !byScheme.has(m[1])) return;
        const [, scheme, id, frag] = m;
        const c = byScheme.get(scheme);
        const entry = reg.get(`${c.id}:${id}`);
        if (!entry) {
          throw new Error(`xref: unknown id "${scheme}:${id}" in ${where(ctx)}. Add the page (frontmatter xref), or list the id as planned in the optional file src/data/xref-planned.yaml (see scripts/registry.mjs).`);
        }
        const target = frag ? `${entry.href.split('#')[0]}#${frag}` : entry.href;
        ctx.setProperty(node, 'href', target);
        ctx.setProperty(node, 'className', [...classes, c.id === 'term' ? 'gl' : 'x']);
        if (c.id === 'term') {
          ctx.setProperty(node, 'dataGl', entry.id);
        } else {
          ctx.setProperty(node, 'dataCat', c.id);
          ctx.setProperty(node, 'dataXref', `${c.id}:${entry.id}`);
        }
        if (entry.planned) ctx.setProperty(node, 'dataPlanned', '');
        if (!node.children.length) {
          // A field (#spec.x), a flag (#--yes) or a key shows as written; a heading
          // anchor (#what-the-doctor-checks) shows as words.
          const part = frag && /^[a-z0-9]+(-[a-z0-9]+)*$/.test(frag) ? frag.replace(/-/g, ' ').replace(/^./, (x) => x.toUpperCase()) : frag;
          const text = frag && c.id !== 'term' ? `${canonicalText(entry)} › ${part}` : canonicalText(entry);
          ctx.appendChild(node, { type: 'text', value: text });
        }
        // The color and icon of the category reach only sighted readers. Name the
        // category for screen readers too, unless the link text names it already
        // ("pk-metal approve" is a command, "mise run lab:up" a lab task).
        if (c.id !== 'term' && !c.prefix && !textOf(node).toLowerCase().includes(c.label.toLowerCase())) {
          ctx.setProperty(node, 'ariaDescription', c.label);
        }
      },
    },
  };
}
