<script lang="ts">
  // What the retrieval index holds (the LLM context), and related hits.
  import type { SearchHit } from '../../api/ftypes';
  import { href } from '../../router.svelte';

  let { mode = '', related = [] }: { mode?: string; related?: SearchHit[] } = $props();

  const SOURCES = ['state repo', 'kb', 'MR discussions', 'trailers', 'lab reports'];
  const hitHref = (h: SearchHit) => {
    const p = h.path ?? h.ref;
    if (h.kind === 'kb' || p.endsWith('.md')) return href('/kb/article', { path: p });
    if (h.kind === 'trailer' || h.kind === 'mr') return href('/history', { q: h.change ?? h.ref });
    return href('/files', { path: p });
  };
</script>

<section class="card idx">
  <h3>Retrieval index</h3>
  <div class="srcs">
    {#each SOURCES as s (s)}<span class="src">{s}</span>{/each}
  </div>
  <p class="muted small">
    Indexed on site{mode ? ` (${mode === 'bm25' ? 'BM25 keywords' : 'local embedder'})` : ''}. Chat and advisors read it; live state is not indexed.
  </p>
  {#if related.length}
    <h4>Related</h4>
    {#each related as h (h.kind + h.ref)}
      <a class="rel" href={hitHref(h)} title={h.snippet}>
        <span class="k">{h.kind}</span>
        <span class="t">{h.title}</span>
      </a>
    {/each}
  {/if}
</section>

<style>
  .card {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px 14px;
    display: grid;
    gap: 6px;
  }
  .idx {
    border: 2px solid color-mix(in srgb, var(--accent-fill) 55%, transparent);
  }
  .srcs {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .src {
    font: 500 12px/1.7 var(--mono);
    padding: 0 7px;
    border-radius: 4px;
    background: var(--grey-100);
    color: var(--grey-600);
  }
  :global(:root[data-theme='dark']) .src {
    background: var(--line);
    color: var(--muted);
  }
  @media (prefers-color-scheme: dark) {
    :global(:root:not([data-theme='light'])) .src {
      background: var(--line);
      color: var(--muted);
    }
  }
  p {
    margin: 0;
  }
  h4 {
    margin: 4px 0 0;
    font-size: 12px;
    color: var(--muted);
  }
  .rel {
    display: flex;
    gap: 8px;
    align-items: baseline;
    text-decoration: none;
    color: var(--ink);
    font-size: 13px;
    min-width: 0;
  }
  .rel .k {
    font: 500 11px var(--mono);
    color: var(--muted);
    width: 48px;
    flex: none;
  }
  .rel .t {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .rel:hover .t {
    color: var(--accent);
  }
</style>
