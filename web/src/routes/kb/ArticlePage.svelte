<script lang="ts">
  import { untrack } from 'svelte';
  // One article: rendered markdown, entities with status, the changes that
  // cite it, and where it is in the retrieval index.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { Article, ArticleSummary, ChangeLink, SearchHit } from '../../lib/api/ftypes';
  import { work } from '../../lib/stores/context.svelte';
  import { isChangeId } from '../../lib/f/fmt';
  import KbList from '../../lib/components/kb/KbList.svelte';
  import KbIndex from '../../lib/components/kb/KbIndex.svelte';
  import Markdown from '../../lib/components/Markdown.svelte';
  import ChangeChip from '../../lib/components/ChangeChip.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const path = $derived(query.get('path') ?? '');
  const ref = $derived(query.get('ref') ?? undefined);
  let q = $state(untrack(() => query.get('q')) ?? '');
  let type = $state('');

  let list = $state<ArticleSummary[]>([]);
  let article = $state<Article | null>(null);
  let related = $state<SearchHit[]>([]);
  let mode = $state('');
  let error = $state<string | null>(null);

  async function loadList() {
    try {
      list = await must(api.GET('/kb', { params: { query: ref ? { ref } : {} } }));
    } catch {
      list = [];
    }
  }

  async function load(p: string) {
    if (!p) return;
    try {
      article = await must(api.GET('/kb/article', { params: { query: { path: p, ...(ref ? { ref } : {}) } } }));
      error = null;
      void loadRelated(article);
    } catch (e) {
      article = null;
      error = errorText(e);
    }
  }

  async function loadRelated(a: Article) {
    try {
      const terms = [a.title, ...a.entities.map((e) => e.name)].join(' ');
      const r = await must(api.GET('/search', { params: { query: { q: terms, limit: 6 } } }));
      mode = r.mode;
      related = r.hits.filter((h) => (h.path ?? h.ref) !== a.path).slice(0, 4);
    } catch {
      related = [];
    }
  }

  $effect(() => {
    void ref;
    void loadList();
  });
  $effect(() => {
    void load(path);
  });

  // Changes in the front matter first, then the ones that touched it.
  const cites = $derived.by(() => {
    if (!article) return [] as ChangeLink[];
    const seen = new Set<string>();
    const out: ChangeLink[] = [];
    for (const c of [...article.changes, ...article.citedBy]) {
      if (seen.has(c.id)) continue;
      seen.add(c.id);
      out.push(c);
    }
    return out;
  });

  const editHref = $derived(
    work.changeId ? href(`/changes/${work.changeId}/files`, { path }) : href('/files', { path }),
  );
  const last = $derived(article?.lastChange);
  const tags = $derived((article?.frontMatter?.tags as string[] | undefined) ?? []);
  const KIND_LABEL: Record<string, string> = {
    Node: 'node',
    Jbod: 'JBOD',
    Rack: 'rack',
    Switch: 'switch',
    Cluster: 'cluster',
    CablePlan: 'cable plan',
  };
  const entHref = (kind: string, name: string) =>
    kind === 'Rack' ? href(`/assets/racks/${encodeURIComponent(name)}`) : href(`/assets/${kind}/${encodeURIComponent(name)}`);
</script>

<div class="kb">
  <aside class="list">
    <KbList articles={list} current={path} bind:q bind:type />
  </aside>

  <article class="art">
    {#if article}
      <div class="path mono">{article.path}{ref ? ` @ ${ref}` : ''}</div>
      <h1>{article.title}</h1>
      <div class="meta muted">
        <span class="badge t-{article.type}">{article.type}</span>
        {#if last}
          <span>
            Last change
            {#if isChangeId(last.id)}
              <a class="mono" href={href(`/changes/${last.id}`)}>{last.id}</a>
            {:else}
              <a class="mono" href={href(`/history/${last.sha ?? last.id}`)}>{(last.sha ?? last.id).slice(0, 7)}</a>
            {/if}
            {#if last.author}{' by '}{last.author}{/if}{#if last.mr}{' · reviewed in MR '}{last.mr}{/if}
          </span>
        {/if}
        <span class="spacer"></span>
        <a class="btn small" href={editHref} title={work.changeId ? `Edit in ${work.changeId}` : 'Open in the files view'}>
          <Glyph name="file" size={13} />{work.changeId ? `Edit in ${work.changeId}` : 'Open file'}
        </a>
      </div>
      {#if article.problems.length}
        <div class="probs">
          {#each article.problems as p, i (i)}<div class="error-box">{p.message}</div>{/each}
        </div>
      {/if}
      <Markdown source={article.body} path={article.path} entities={article.entities} dropTitle={article.title} />
      {#if tags.length}
        <div class="tags">{#each tags as t (t)}<span class="badge">#{t}</span>{/each}</div>
      {/if}
    {:else if error}
      <div class="error-box">{error}</div>
    {:else if !path}
      <p class="muted">Pick an article.</p>
    {:else}
      <p class="muted">Loading…</p>
    {/if}
  </article>

  <aside class="links">
    <h3 class="lh">Links</h3>
    {#if article}
      <section class="card">
        <h3>Entities</h3>
        {#each article.entities as e (e.name + e.field)}
          <div class="ln">
            <Glyph name={e.kind === 'Cluster' ? 'hex' : e.kind === 'Jbod' ? 'disk' : 'server'} size={14} />
            {#if e.exists}
              <a class="mono" href={entHref(e.kind, e.name)}>{e.name}</a>
            {:else}
              <span class="mono">{e.name}</span>
            {/if}
            <span class="muted small">{KIND_LABEL[e.kind] ?? e.kind}</span>
            <span class="spacer"></span>
            <span class="st {e.exists ? 'ok' : 'bad'}">{e.exists ? 'ok' : 'missing'}</span>
          </div>
        {:else}
          <p class="muted small">No entities in the front matter.</p>
        {/each}
      </section>

      <section class="card">
        <h3>Changes that cite it</h3>
        {#each cites as c (c.id)}
          <div class="ln"><ChangeChip change={c} title /></div>
        {:else}
          <p class="muted small">No change cites it yet.</p>
        {/each}
      </section>

      <KbIndex {mode} {related} />
    {/if}
  </aside>
</div>

<style>
  .kb {
    display: grid;
    grid-template-columns: 290px minmax(0, 1fr) 320px;
    height: 100%;
    min-height: 0;
  }
  .list {
    border-right: 1px solid var(--line);
    padding: 12px;
    min-height: 0;
    overflow: hidden;
  }
  .art {
    padding: 18px 28px 40px;
    overflow-y: auto;
    min-width: 0;
    background: var(--panel);
  }
  .path {
    font-size: 12px;
    color: var(--muted);
  }
  h1 {
    font-size: 24px;
    margin: 4px 0 6px;
  }
  .meta {
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 13px;
    margin-bottom: 14px;
    flex-wrap: wrap;
  }
  .t-runbook {
    background: var(--accent-soft);
    color: var(--accent);
  }
  .t-incident {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .t-lab-report {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .t-design {
    background: var(--info-soft);
    color: var(--info);
  }
  .probs {
    display: grid;
    gap: 4px;
    margin-bottom: 10px;
  }
  .meta a.btn {
    text-decoration: none;
  }
  .tags {
    display: flex;
    gap: 6px;
    margin-top: 16px;
  }
  .links {
    border-left: 1px solid var(--line);
    padding: 12px;
    display: grid;
    gap: 12px;
    align-content: start;
    overflow-y: auto;
  }
  .lh {
    padding: 2px 4px;
  }
  .card {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px 14px;
    display: grid;
    gap: 4px;
  }
  .card h3 {
    margin-bottom: 4px;
  }
  .ln {
    display: flex;
    align-items: center;
    gap: 7px;
    font-size: 13px;
    padding: 3px 0;
    min-width: 0;
  }
  .ln :global(.cc) {
    flex: 1;
  }
  .ln > :global(svg) {
    color: var(--muted);
  }
  .st {
    font-size: 11px;
    font-weight: 600;
    padding: 0 7px;
    border-radius: 4px;
  }
  .st.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .st.bad {
    background: var(--bad-soft);
    color: var(--bad);
  }
  @media (max-width: 1200px) {
    .kb {
      grid-template-columns: 260px minmax(0, 1fr);
    }
    .links {
      grid-column: 2;
      border-left: 0;
    }
  }
  @media (max-width: 720px) {
    .kb {
      grid-template-columns: minmax(0, 1fr);
      height: auto;
    }
    .list {
      display: none;
    }
    .links {
      grid-column: 1;
    }
  }
</style>
