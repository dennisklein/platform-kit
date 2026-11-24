// Helpers for the inline SVG diagrams (Figure, Explainer and the components that
// draw their own <svg>).

/** The ids of the shared markers and patterns. SvgDefs.astro defines each once per page. */
export const sharedDefIds = ['pk-arrow', 'pk-hatch'];

/**
 * Removes the local copies of the shared defs (`<marker id="pk-arrow">`,
 * `<pattern id="pk-hatch">`) and a `<defs>` that is then empty. Two copies of one id
 * on a page are invalid HTML; the copy in SvgDefs.astro serves every diagram.
 */
export function dropSharedDefs(html: string): string {
  let out = html;
  for (const id of sharedDefIds) {
    out = out.replace(new RegExp(`<(marker|pattern)\\b[^>]*\\bid="${id}"[^>]*>[\\s\\S]*?</\\1>`, 'g'), '');
  }
  return out.replace(/<defs>\s*<\/defs>/g, '');
}

/**
 * Gives each `<svg role="img">` that contains a link the role "group". The role
 * "img" hides the children from assistive technology, so a screen reader would not
 * announce a link that the keyboard can focus (WCAG 4.1.2). A group keeps the
 * title (aria-labelledby) and exposes the links.
 */
export function linkRole(html: string): string {
  const open = /<svg\b[^>]*>/g;
  let out = '';
  let last = 0;
  for (let m = open.exec(html); m; m = open.exec(html)) {
    if (m.index < last) continue;
    const end = closeOf(html, m.index);
    const body = html.slice(m.index, end);
    const tag = m[0];
    const fixed = /\srole="img"/.test(tag) && /<a\s/.test(body) ? tag.replace(/\srole="img"/, ' role="group"') : tag;
    out += html.slice(last, m.index) + fixed + body.slice(tag.length);
    last = end;
    open.lastIndex = end;
  }
  return out + html.slice(last);
}

/** The index after the `</svg>` that closes the `<svg>` at `start`, nested SVGs included. */
function closeOf(html: string, start: number): number {
  const tags = /<svg\b|<\/svg>/g;
  tags.lastIndex = start;
  let depth = 0;
  for (let m = tags.exec(html); m; m = tags.exec(html)) {
    depth += m[0] === '</svg>' ? -1 : 1;
    if (depth === 0) return m.index + m[0].length;
  }
  return html.length;
}

/** A stable id from a text: the same title gives the same id on each build. */
export function stableId(prefix: string, text: string): string {
  let h = 2166136261;
  for (let i = 0; i < text.length; i++) h = Math.imul(h ^ text.charCodeAt(i), 16777619);
  return `${prefix}-${(h >>> 0).toString(36)}`;
}
