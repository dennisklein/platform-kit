<script lang="ts">
  import { untrack } from 'svelte';
  // Knowledge base: article list with search; the right side gives an
  // overview by type. Opening an article goes to /kb/article?path=.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { ArticleSummary } from '../../lib/api/ftypes';
  import KbList from '../../lib/components/kb/KbList.svelte';
  import KbIndex from '../../lib/components/kb/KbIndex.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  let q = $state(untrack(() => query.get('q')) ?? '');
  let type = $state(untrack(() => query.get('type')) ?? '');
  let list = $state<ArticleSummary[]>([]);
  let error = $state<string | null>(null);
  let mode = $state('');

  $effect(() => {
    void (async () => {
      try {
        list = await must(api.GET('/kb'));
        error = null;
      } catch (e) {
        error = errorText(e);
      }
      try {
        mode = (await must(api.GET('/search', { params: { query: { q: 'runbook', limit: 1 } } }))).mode;
      } catch {
        mode = '';
      }
    })();
  });

  const TYPES: [string, string, string][] = [
    ['runbook', 'Runbooks', 'Steps for operators, with pk commands.'],
    ['design', 'Design', 'Why the platform is the way it is.'],
    ['reference', 'Reference', 'Facts: slot numbers, layouts.'],
    ['incident', 'Incidents', 'What happened, break-glass use.'],
    ['lab-report', 'Lab reports', 'Written by change labs, one per run.'],
  ];
  const count = (t: string) => list.filter((a) => a.type === t).length;
</script>

<div class="kb">
  <aside class="list">
    <KbList articles={list} bind:q bind:type />
  </aside>
  <section class="main">
    <h1>Knowledge</h1>
    <p class="muted lead">
      Articles are Markdown files under <code>kb/</code> with front matter. They link entities and changes, and they change
      in changes like everything else.
    </p>
    {#if error}<div class="error-box">{error}</div>{/if}
    <div class="types">
      {#each TYPES as [t, label, help] (t)}
        <button class="tcard" class:on={type === t} onclick={() => (type = type === t ? '' : t)}>
          <span class="n">{count(t)}</span>
          <span class="l">{label}</span>
          <span class="h muted">{help}</span>
        </button>
      {/each}
    </div>
    <div class="recent">
      <h3>Recently changed</h3>
      {#each list.filter((a) => a.lastChange).sort((a, b) => (b.lastChange?.date ?? '').localeCompare(a.lastChange?.date ?? '')).slice(0, 6) as a (a.path)}
        <a class="ra" href={href('/kb/article', { path: a.path })}>
          <Glyph name="book" size={14} />
          <span class="t">{a.title}</span>
          <span class="muted small mono">{a.lastChange?.id}</span>
        </a>
      {:else}
        {#each list.slice(0, 6) as a (a.path)}
          <a class="ra" href={href('/kb/article', { path: a.path })}>
            <Glyph name="book" size={14} /><span class="t">{a.title}</span><span class="badge">{a.type}</span>
          </a>
        {/each}
      {/each}
    </div>
    <div class="idxw"><KbIndex {mode} /></div>
  </section>
</div>

<style>
  .kb {
    display: grid;
    grid-template-columns: 290px minmax(0, 1fr);
    height: 100%;
    min-height: 0;
  }
  .list {
    border-right: 1px solid var(--line);
    padding: 12px;
    min-height: 0;
    overflow: hidden;
  }
  .main {
    padding: 18px 28px 40px;
    overflow-y: auto;
    display: grid;
    gap: 16px;
    align-content: start;
  }
  .lead {
    margin: -8px 0 0;
    max-width: 70ch;
  }
  .types {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(170px, 1fr));
    gap: 10px;
  }
  .tcard {
    display: grid;
    gap: 2px;
    text-align: left;
    padding: 12px 14px;
    border: 1px solid var(--line);
    border-radius: var(--radius);
    background: var(--panel);
  }
  .tcard.on {
    border-color: var(--accent-fill);
    box-shadow: 0 0 0 1px var(--accent-fill);
  }
  .tcard .n {
    font-size: 22px;
    font-weight: 700;
  }
  .tcard .l {
    font-weight: 600;
  }
  .tcard .h {
    font-size: 12px;
  }
  .recent {
    display: grid;
    gap: 2px;
  }
  .recent h3 {
    margin-bottom: 4px;
  }
  .ra {
    display: flex;
    gap: 8px;
    align-items: center;
    padding: 5px 0;
    color: var(--ink);
    text-decoration: none;
    font-size: 14px;
  }
  .ra .t {
    flex: 1;
  }
  .ra:hover .t {
    color: var(--accent);
  }
  .idxw {
    max-width: 520px;
  }
  @media (max-width: 720px) {
    .kb {
      grid-template-columns: minmax(0, 1fr);
      height: auto;
    }
    .list {
      border-right: 0;
      border-bottom: 1px solid var(--line);
      max-height: 40vh;
      overflow: auto !important;
    }
  }
</style>
