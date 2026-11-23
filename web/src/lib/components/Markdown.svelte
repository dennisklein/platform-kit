<script lang="ts" module>
  import { Marked, type Tokens } from 'marked';

  const esc = (s: string) =>
    s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

  /** Resolve a relative link of an article (../hardware/x.md) to a repo path. */
  export function resolvePath(base: string, rel: string): string {
    const parts = base.split('/').slice(0, -1);
    for (const p of rel.split('/')) {
      if (p === '..') parts.pop();
      else if (p && p !== '.') parts.push(p);
    }
    return parts.join('/');
  }

  const SHELL = /^(pk|kubectl|ceph|talosctl|flux|git|docker|curl|helm|cilium)\b/;

  // A copy button on each code block; one click handler on the article
  // copies the code without the shell prompts.
  const COPY_ICON =
    '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 9h11v11H9zM5 15H4V4h11v1"/></svg>';
  const COPY_BUTTON = `<button type="button" class="cpy" data-copy title="Copy" aria-label="Copy code">${COPY_ICON}<span>Copy</span></button>`;

  function makeMarked(base: string): Marked {
    return new Marked({
      gfm: true,
      breaks: false,
      renderer: {
        // Raw HTML in repo files is shown as text, never rendered.
        html({ text }: Tokens.HTML | Tokens.Tag) {
          return esc(text);
        },
        code({ text, lang }: Tokens.Code) {
          const shell = !lang ? text.split('\n').every((l) => !l.trim() || SHELL.test(l.trim())) : /^(sh|bash|shell|console)$/.test(lang);
          const body = text
            .split('\n')
            .map((l) => (shell && l.trim() ? `<span class="prompt">$ </span>${esc(l)}` : esc(l)))
            .join('\n');
          return `<pre class="code${shell ? ' shell' : ''}"${lang ? ` data-lang="${esc(lang)}"` : ''}>${COPY_BUTTON}<code>${body}</code></pre>`;
        },
        link({ href, title, tokens }: Tokens.Link) {
          const inner = this.parser.parseInline(tokens);
          const t = title ? ` title="${esc(title)}"` : '';
          if (/^https?:\/\//.test(href)) return `<a href="${esc(href)}"${t} target="_blank" rel="noreferrer">${inner}</a>`;
          if (/^[a-z]+:/i.test(href)) return inner; // javascript:, data: and others
          const [p, frag] = href.split('#');
          const path = p ? resolvePath(base, p) : base;
          const target = path.endsWith('.md')
            ? `#/kb/article?path=${encodeURIComponent(path)}`
            : `#/files?path=${encodeURIComponent(path)}`;
          return `<a href="${target}"${t}${frag ? ` data-frag="${esc(frag)}"` : ''}>${inner}</a>`;
        },
      },
    });
  }
</script>

<script lang="ts">
  // Markdown of a KB article, rendered without raw HTML. Entity names in the
  // text become chips that link to the entity (with ok/missing status).
  import type { EntityRef } from '../api/ftypes';
  import { copyText } from '../f/copy';

  let {
    source,
    path = 'kb/x.md',
    entities = [],
    dropTitle,
  }: { source: string; path?: string; entities?: EntityRef[]; dropTitle?: string } = $props();

  let el: HTMLDivElement | undefined = $state();

  const html = $derived.by(() => {
    let src = source ?? '';
    if (dropTitle) src = src.replace(/^\s*#\s+(.+)\n/, (m, t: string) => (t.trim() === dropTitle.trim() ? '' : m));
    const out = makeMarked(path).parse(src, { async: false }) as string;
    // Key/value tables ("| | |" header) get no empty header row.
    return out.replace(/<thead>\s*<tr>(?:\s*<th[^>]*>\s*<\/th>)+\s*<\/tr>\s*<\/thead>/g, '');
  });

  const entityHref = (e: EntityRef) =>
    e.kind === 'Rack' ? `#/assets/racks/${encodeURIComponent(e.name)}` : `#/assets/${e.kind}/${encodeURIComponent(e.name)}`;

  // Wrap entity names in text nodes and inline code (not inside links or
  // code blocks) into chips.
  $effect(() => {
    void html;
    if (!el || !entities.length) return;
    const byName = new Map(entities.map((e) => [e.name, e]));
    const names = [...byName.keys()].sort((a, b) => b.length - a.length).map((n) => n.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
    const re = new RegExp(`(^|[^\\w-])(${names.join('|')})(?![\\w-])`, 'g');
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    const nodes: Text[] = [];
    while (walker.nextNode()) {
      const n = walker.currentNode as Text;
      if (n.parentElement?.closest('a, pre, .ent')) continue;
      if (re.test(n.data)) nodes.push(n);
      re.lastIndex = 0;
    }
    for (const n of nodes) {
      const parent = n.parentElement;
      // Inline code that is only an entity name becomes the chip itself.
      if (parent?.tagName === 'CODE' && byName.has(n.data.trim())) {
        const e = byName.get(n.data.trim())!;
        const a = chip(e);
        parent.replaceWith(a);
        continue;
      }
      if (parent?.tagName === 'CODE') continue;
      const frag = document.createDocumentFragment();
      let last = 0;
      n.data.replace(re, (m, pre: string, name: string, off: number) => {
        frag.append(n.data.slice(last, off) + pre);
        frag.append(chip(byName.get(name)!));
        last = off + m.length;
        return m;
      });
      frag.append(n.data.slice(last));
      n.replaceWith(frag);
    }
  });

  // Copy the code of a block (the "$ " prompts are not part of it).
  async function onclick(ev: MouseEvent) {
    const btn = (ev.target as HTMLElement | null)?.closest<HTMLButtonElement>('button[data-copy]');
    const code = btn?.parentElement?.querySelector('code');
    if (!btn || !code) return;
    ev.preventDefault();
    const c = code.cloneNode(true) as HTMLElement;
    c.querySelectorAll('.prompt').forEach((p) => p.remove());
    const ok = await copyText(c.textContent ?? '');
    const label = btn.querySelector('span');
    if (!label) return;
    label.textContent = ok ? 'Copied' : 'Select and copy';
    btn.classList.toggle('ok', ok);
    setTimeout(() => {
      label.textContent = 'Copy';
      btn.classList.remove('ok');
    }, 1600);
  }

  function chip(e: EntityRef): HTMLAnchorElement {
    const a = document.createElement('a');
    a.className = `ent${e.exists ? '' : ' missing'}`;
    a.href = entityHref(e);
    a.title = `${e.kind} ${e.name}${e.exists ? '' : ' (not found)'}`;
    a.textContent = e.name;
    return a;
  }
</script>

{#key html}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions (the copy buttons inside are buttons) -->
  <div class="md" bind:this={el} {onclick}>{@html html}</div>
{/key}

<style>
  .md {
    line-height: 1.6;
    font-size: 14.5px;
    max-width: 78ch;
  }
  .md :global(h1) {
    font-size: 22px;
    margin: 0 0 10px;
  }
  .md :global(h2) {
    font-size: 16px;
    margin: 22px 0 6px;
  }
  .md :global(h3) {
    font-size: 14px;
    text-transform: none;
    letter-spacing: 0;
    color: var(--ink);
    margin: 16px 0 4px;
  }
  .md :global(p),
  .md :global(ul),
  .md :global(ol) {
    margin: 0 0 10px;
  }
  .md :global(li) {
    margin: 2px 0;
  }
  /* No programming ligatures: "bgp-<rack>" must not turn into an arrow. */
  .md :global(code),
  .md :global(pre) {
    font-variant-ligatures: none;
  }
  .md :global(:not(pre) > code) {
    background: var(--code-bg);
    border: 1px solid var(--line);
    padding: 0 4px;
    border-radius: 4px;
    font-size: 12.5px;
    /* Long paths wrap inside a narrow bubble instead of overflowing it. */
    overflow-wrap: anywhere;
  }
  .md :global(pre.code) {
    position: relative;
    margin: 6px 0 12px;
    background: var(--gsi-navy);
    color: #e7e9ee;
    border: 0;
    border-radius: 6px;
    padding: 12px 16px;
    font-size: 13px;
    /* Long commands wrap instead of running out of the box. */
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .md :global(pre.code code) {
    display: block;
    padding-right: 64px;
  }
  .md :global(.cpy) {
    position: absolute;
    top: 7px;
    right: 7px;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    border: 1px solid #454b58;
    background: #2a2f39;
    color: #c9ccd2;
    border-radius: 5px;
    padding: 2px 7px;
    font: 500 11.5px/1.4 var(--sans);
    opacity: 0.75;
    transition: opacity 0.15s;
  }
  .md :global(pre.code:hover .cpy),
  .md :global(.cpy:focus-visible) {
    opacity: 1;
  }
  .md :global(.cpy:hover) {
    background: #343a46;
    color: #fff;
  }
  .md :global(.cpy.ok) {
    color: #8bc97a;
    border-color: #5c8a4f;
    opacity: 1;
  }
  .md :global(.prompt) {
    color: var(--gsi-orange);
    user-select: none;
  }
  .md :global(table) {
    border-collapse: collapse;
    margin: 6px 0 12px;
    font-size: 13px;
  }
  .md :global(th),
  .md :global(td) {
    border: 1px solid var(--line);
    padding: 4px 8px;
    text-align: left;
  }
  .md :global(th) {
    background: var(--panel-2);
  }
  .md :global(blockquote) {
    margin: 0 0 10px;
    padding: 2px 12px;
    border-left: 3px solid var(--line-strong);
    color: var(--muted);
  }
  .md :global(a.ent) {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font: 600 12.5px/1.5 var(--mono);
    padding: 0 6px;
    border-radius: 4px;
    background: var(--info-soft);
    color: var(--info);
    text-decoration: none;
  }
  .md :global(a.ent::before) {
    content: '';
    width: 10px;
    height: 8px;
    border: 1.5px solid currentColor;
    border-radius: 2px;
  }
  .md :global(a.ent.missing) {
    background: var(--bad-soft);
    color: var(--bad);
  }
</style>
