<script lang="ts">
  // Article list with search and type filter (left column of the KB pages).
  // The search filters titles and entities, and asks the retrieval index.
  import { api, must } from '../../api/client';
  import type { ArticleSummary, SearchHit } from '../../api/ftypes';
  import { href, router } from '../../router.svelte';
  import Glyph from '../Glyph.svelte';

  let {
    articles,
    current,
    q = $bindable(''),
    type = $bindable(''),
  }: { articles: ArticleSummary[]; current?: string; q?: string; type?: string } = $props();

  const TYPES = ['runbook', 'design', 'reference', 'incident', 'lab-report'];
  const dir = (p: string) => p.split('/').slice(0, -1).join('/');

  const words = $derived(q.toLowerCase().split(/\s+/).filter(Boolean));
  const shown = $derived(
    articles
      .filter((a) => !type || a.type === type)
      .filter((a) => {
        if (!words.length) return true;
        const hay = [a.title, a.path, a.type, ...(a.entities ?? []), ...(a.changes ?? [])].join(' ').toLowerCase();
        return words.every((w) => hay.includes(w));
      }),
  );

  let hits = $state<SearchHit[]>([]);
  let mode = $state('');
  let timer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    const text = q.trim();
    clearTimeout(timer);
    if (text.length < 3) {
      hits = [];
      return;
    }
    timer = setTimeout(async () => {
      try {
        const r = await must(api.GET('/search', { params: { query: { q: text, limit: 8 } } }));
        mode = r.mode;
        const listed = new Set(shown.map((a) => a.path));
        hits = r.hits.filter((h) => !(h.kind === 'kb' && listed.has(h.path ?? h.ref)));
      } catch {
        hits = [];
      }
    }, 250);
  });

  function keep(e: Event) {
    e.preventDefault();
    const params = new URLSearchParams(router.query);
    if (q) params.set('q', q);
    else params.delete('q');
    router.replace(`${router.path}?${params}`);
  }

  const hitHref = (h: SearchHit) => {
    const p = h.path ?? h.ref;
    if (h.kind === 'kb' || (h.kind === 'lab' && p.endsWith('.md'))) return href('/kb/article', { path: p, q: q || undefined });
    if (h.kind === 'trailer' || h.kind === 'mr') return href('/history', { q: h.change ?? h.ref });
    if (h.kind === 'lab') return href(`/labs/runs/${encodeURIComponent(h.ref)}`);
    return href('/files', { path: p });
  };
</script>

<div class="kblist">
  <form class="search" onsubmit={keep}>
    <Glyph name="search" size={15} />
    <input class="input" placeholder="disk sn-05" bind:value={q} aria-label="Search the knowledge base" />
  </form>
  <div class="types" role="group" aria-label="Type">
    <button class="chip-filter" aria-pressed={!type} onclick={() => (type = '')}>all</button>
    {#each TYPES as t (t)}
      <button class="chip-filter" aria-pressed={type === t} onclick={() => (type = type === t ? '' : t)}>{t}</button>
    {/each}
  </div>

  <nav class="items" aria-label="Articles">
    {#each shown as a (a.path)}
      <a class="item" class:active={a.path === current} href={href('/kb/article', { path: a.path, q: q || undefined })}>
        <span class="t">{a.title}</span>
        <span class="m mono">{a.type} · {dir(a.path)}</span>
      </a>
    {:else}
      <p class="muted small pad">No article matches.</p>
    {/each}

    {#if hits.length}
      <div class="idx">
        <h3>From the index <span class="badge">{mode}</span></h3>
        {#each hits as h (h.kind + h.ref)}
          <a class="item hit" href={hitHref(h)} title={h.snippet}>
            <span class="t">{h.title}</span>
            <span class="m mono">{h.kind} · {h.path ?? h.ref}</span>
            <span class="snip">{h.snippet}</span>
          </a>
        {/each}
      </div>
    {/if}
  </nav>
</div>

<style>
  .kblist {
    display: grid;
    grid-template-rows: auto auto minmax(0, 1fr);
    gap: 8px;
    min-height: 0;
    height: 100%;
  }
  .search {
    display: flex;
    align-items: center;
    gap: 8px;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    padding: 0 10px;
    background: var(--panel);
    color: var(--muted);
  }
  .search .input {
    border: 0;
    flex: 1;
    padding: 7px 0;
    background: transparent;
    outline: none;
  }
  .types {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .items {
    overflow-y: auto;
    display: grid;
    align-content: start;
    gap: 2px;
    min-height: 0;
  }
  .item {
    display: grid;
    gap: 2px;
    padding: 8px 10px;
    border-radius: 6px;
    border-left: 3px solid transparent;
    color: var(--ink);
    text-decoration: none;
  }
  .item:hover {
    background: var(--panel-2);
  }
  .item.active {
    background: var(--panel);
    border-left-color: var(--accent-fill);
    box-shadow: var(--shadow);
  }
  .t {
    font-weight: 600;
    font-size: 14px;
    line-height: 1.3;
  }
  .m {
    font-size: 11.5px;
    color: var(--muted);
  }
  .idx {
    border-top: 1px solid var(--line);
    margin-top: 6px;
    padding-top: 8px;
    display: grid;
    gap: 2px;
  }
  .idx h3 {
    padding: 0 10px 4px;
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .hit .t {
    font-weight: 500;
    font-size: 13px;
  }
  .snip {
    font-size: 12px;
    color: var(--muted);
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .pad {
    padding: 8px 10px;
    margin: 0;
  }
</style>
