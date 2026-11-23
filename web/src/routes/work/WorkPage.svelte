<script lang="ts">
  // Work board: items in planned / in progress / in review / done, derived
  // from the states of their linked changes; milestone progress; item detail.
  import type { Params } from '../../lib/router.svelte';
  import { router, href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { WorkBoard, WorkItem, WorkStatus, FileContent } from '../../lib/api/ftypes';
  import { events } from '../../lib/stores/events.svelte';
  import { isChangeId } from '../../lib/f/fmt';
  import ChangeChip from '../../lib/components/ChangeChip.svelte';
  import YamlView from '../../lib/components/YamlView.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  let board = $state<WorkBoard | null>(null);
  let error = $state<string | null>(null);
  let file = $state<FileContent | null>(null);

  const COLS: [WorkStatus, string][] = [
    ['planned', 'Planned'],
    ['in-progress', 'In progress'],
    ['in-review', 'In review'],
    ['done', 'Done'],
  ];

  async function load() {
    try {
      board = await must(api.GET('/work'));
      error = null;
    } catch (e) {
      error = errorText(e);
    }
  }
  $effect(() => {
    void load();
  });

  // Statuses follow the changes: reload on change, forge and flux events.
  $effect(() => {
    let t: ReturnType<typeof setTimeout> | undefined;
    const later = () => {
      clearTimeout(t);
      t = setTimeout(() => void load(), 400);
    };
    const offs = ['change.created', 'change.updated', 'forge.updated', 'flux.updated'].map((e) => events.on(e as any, later));
    return () => {
      clearTimeout(t);
      offs.forEach((o) => o());
    };
  });

  const milestoneId = $derived(query.get('milestone') ?? board?.milestones[0]?.id ?? '');
  const milestone = $derived(board?.milestones.find((m) => m.id === milestoneId));
  const items = $derived((board?.items ?? []).filter((i) => !milestoneId || i.milestone === milestoneId || !i.milestone));
  const done = $derived(items.filter((i) => i.status === 'done').length);
  const pct = $derived(items.length ? Math.round((done / items.length) * 100) : 0);

  const selId = $derived(query.get('item') ?? undefined);
  const sel = $derived(board?.items.find((i) => i.id === selId));

  async function loadFile(it: WorkItem | undefined) {
    file = null;
    if (!it?.path) return;
    try {
      file = await must(api.GET('/refs/{ref}/file', { params: { path: { ref: 'main' }, query: { path: it.path } } }));
    } catch {
      file = null;
    }
  }
  $effect(() => {
    void loadFile(sel);
  });

  function pick(it: WorkItem) {
    const q = new URLSearchParams(router.query);
    if (selId === it.id) q.delete('item');
    else q.set('item', it.id);
    router.navigate(`/work${q.size ? `?${q}` : ''}`);
  }

  const linked = (it: WorkItem) => it.changes.find((c) => c.id === it.statusFrom) ?? it.changes[it.changes.length - 1];
</script>

<div class="work" class:withdetail={!!sel}>
  <section class="boardcol">
    <header class="mh">
      <Glyph name="board" size={20} />
      <h1>{milestone?.title ?? 'Work'}</h1>
      {#if milestone}<span class="mono muted small">milestone {milestone.id}</span>{/if}
      {#if board && board.milestones.length > 1}
        <select class="select" value={milestoneId} onchange={(e) => router.navigate(`/work?milestone=${(e.target as HTMLSelectElement).value}`)}>
          {#each board.milestones as m (m.id)}<option value={m.id}>{m.title}</option>{/each}
        </select>
      {/if}
      <span class="spacer"></span>
      {#if items.length}
        <div class="progress" title="{done} of {items.length} items done ({pct}%)">
          <div class="bar"><div class="fill" style:width="{pct}%"></div></div>
          <span class="mono">{done} of {items.length} done</span>
        </div>
      {/if}
    </header>
    {#if milestone?.description || milestone?.due}
      <p class="muted desc">
        {milestone.description}{#if milestone.due}<span class="due">{' · due '}{milestone.due}</span>{/if}
      </p>
    {/if}
    {#if error}<div class="error-box">{error}</div>{/if}

    <div class="cols">
      {#each COLS as [st, label] (st)}
        {@const col = items.filter((i) => i.status === st)}
        <div class="col">
          <h3>{label} <span class="n">{col.length}</span></h3>
          {#each col as it (it.id)}
            {@const c = linked(it)}
            <div class="card" class:sel={it.id === selId}>
              <button class="open" onclick={() => pick(it)} aria-expanded={it.id === selId}>
                <span class="t">{it.title}</span>
                <span class="meta mono">{it.id}{it.owner ? `  ${it.owner}` : ''}</span>
              </button>
              {#if c}
                <span class="lnk"><ChangeChip change={c} /></span>
              {/if}
            </div>
          {:else}
            <div class="empty-col"></div>
          {/each}
        </div>
      {/each}
    </div>
  </section>

  {#if sel}
    <aside class="detail">
      <header class="dh">
        <h3>Work item</h3>
        <span class="mono id">{sel.id}</span>
        <button class="btn ghost small" onclick={() => pick(sel)} title="Close"><Glyph name="x" size={14} /></button>
      </header>
      <h2>{sel.title}</h2>
      <div class="yaml">
        <div class="yh mono">{sel.path}</div>
        {#if file}
          <YamlView text={file.content} />
        {:else}
          <p class="muted small">Loading…</p>
        {/if}
      </div>

      <section class="box">
        <h3>Status</h3>
        <div class="status">
          <span class="ws ws-{sel.status}">{sel.status}</span>
          {#if sel.statusFrom}
            <span class="from">
              <Glyph name="branch" size={14} />
              from <a class="mono" href={href(isChangeId(sel.statusFrom) ? `/changes/${sel.statusFrom}` : `/history/${sel.statusFrom}`)}>{sel.statusFrom}</a>
            </span>
          {:else}
            <span class="from muted">no linked change yet</span>
          {/if}
          <span class="spacer"></span>
          <span class="derived mono">derived</span>
        </div>
      </section>

      {#if sel.changes.length}
        <section class="box">
          <h3>Changes</h3>
          {#each sel.changes as c (c.id)}<div class="row-c"><ChangeChip change={c} title /></div>{/each}
        </section>
      {/if}

      {#if sel.assets?.length}
        <section class="box">
          <h3>Assets</h3>
          <div class="chips">
            {#each sel.assets as a (a)}
              <a class="mono chip" href={href('/assets', { q: a })}>{a}</a>
            {/each}
          </div>
        </section>
      {/if}

      {#if sel.kb?.length}
        <section class="box">
          <h3>Knowledge</h3>
          {#each sel.kb as k (k)}
            <a class="kb" href={href('/kb/article', { path: k })}><Glyph name="book" size={14} /> {k}</a>
          {/each}
        </section>
      {/if}
      {#if !sel.changes.length}
        <p class="muted small">Link a change: create one with this work item, or add <code>Pk-Work-Item: {sel.id}</code>.</p>
      {/if}
    </aside>
  {/if}
</div>

<style>
  .work {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    height: 100%;
    min-height: 0;
  }
  .work.withdetail {
    grid-template-columns: minmax(0, 1fr) 380px;
  }
  /* The board follows its own width (browser zoom, item panel): four
     columns down to 760 px, each scrolling on its own; below that two
     columns as high as their cards, and the board scrolls. */
  .boardcol {
    container-type: inline-size;
    padding: 16px 20px;
    display: grid;
    grid-template-rows: auto auto auto minmax(0, 1fr);
    gap: 10px;
    min-height: 0;
    overflow: auto;
  }
  .mh {
    display: flex;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .mh :global(svg) {
    color: var(--accent);
  }
  .desc {
    margin: -4px 0 0;
    font-size: 13px;
  }
  .progress {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 5px 10px;
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--panel);
    font-size: 13px;
  }
  .bar {
    width: 220px;
    height: 8px;
    border-radius: 4px;
    background: var(--line);
    overflow: hidden;
  }
  .fill {
    height: 100%;
    background: var(--ok);
    transition: width 0.4s;
  }
  .cols {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 12px;
    min-height: 0;
  }
  .col {
    background: var(--panel-2);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    min-height: 0;
    overflow-y: auto;
  }
  .col > :global(*) {
    flex: none;
  }
  .col h3 {
    padding: 0 2px 2px;
  }
  .n {
    color: var(--muted);
    font-weight: 400;
    margin-left: 4px;
  }
  .card {
    display: grid;
    gap: 6px;
    text-align: left;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px 12px;
    box-shadow: 0 1px 2px rgb(0 0 0 / 0.04);
  }
  .card:hover {
    border-color: var(--line-strong);
  }
  .open {
    display: grid;
    gap: 6px;
    text-align: left;
    border: 0;
    background: none;
    padding: 0;
  }
  .card.sel {
    border-color: var(--accent-fill);
    box-shadow: 0 0 0 1px var(--accent-fill), var(--shadow);
  }
  .card .t {
    font-weight: 600;
    font-size: 14.5px;
    line-height: 1.3;
  }
  .card .meta {
    font-size: 12px;
    color: var(--muted);
    white-space: pre;
  }
  .lnk {
    border-top: 1px dashed var(--line);
    padding-top: 6px;
  }
  .empty-col {
    border: 1.5px dashed var(--line-strong);
    border-radius: var(--radius);
    height: 110px;
  }
  .detail {
    min-width: 0;
    border-left: 1px solid var(--line);
    background: var(--panel);
    padding: 12px 16px 24px;
    overflow-y: auto;
    display: grid;
    gap: 12px;
    align-content: start;
  }
  .dh {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .dh .id {
    margin-left: auto;
    font-weight: 600;
  }
  .yaml {
    min-width: 0;
    overflow-x: auto;
    border-left: 4px solid var(--accent-fill);
    background: var(--code-bg);
    border-radius: 0 var(--radius-s) var(--radius-s) 0;
    padding: 10px 14px;
  }
  .yaml :global(pre) {
    border: 0;
    background: transparent;
    padding: 0;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .yh {
    font-size: 11.5px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--accent);
    margin-bottom: 6px;
    font-weight: 600;
  }
  .box {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px 14px;
    display: grid;
    gap: 6px;
  }
  .status {
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 14px;
  }
  .from {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .from :global(svg) {
    color: var(--accent);
  }
  .derived {
    font-size: 11.5px;
    color: var(--muted);
  }
  .ws {
    font-size: 11.5px;
    font-weight: 600;
    padding: 1px 8px;
    border-radius: 4px;
    background: var(--line);
    color: var(--muted);
  }
  .ws-in-progress {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .ws-in-review {
    background: var(--info-soft);
    color: var(--info);
  }
  .ws-done {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .chip {
    font-size: 12px;
    padding: 0 6px;
    border-radius: 4px;
    background: var(--info-soft);
    color: var(--info);
    text-decoration: none;
  }
  .kb {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
  }
  p {
    margin: 0;
  }
  @container (max-width: 760px) {
    .cols {
      grid-template-columns: repeat(2, minmax(0, 1fr));
      align-items: start;
    }
    .col {
      overflow: visible;
      min-height: 160px;
    }
  }
  @media (max-width: 640px) {
    .cols {
      grid-template-columns: minmax(0, 1fr);
    }
    .work.withdetail {
      grid-template-columns: minmax(0, 1fr);
    }
    .boardcol {
      padding: 12px 16px;
    }
  }
</style>
