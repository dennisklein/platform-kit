<script lang="ts">
  // History of main: one row per merge (change), break-glass and direct
  // commits; filters; detail with trailers, semantic diff, time travel.
  import { untrack } from 'svelte';
  import { router, href } from '../../router.svelte';
  import { api, must, errorText } from '../../api/client';
  import type { HistoryDetail, HistoryEntry } from '../../api/ftypes';
  import { events } from '../../stores/events.svelte';
  import { fromParams, toParams, type HistoryQuery } from '../../f/query';
  import { shortDate, dateTime, short, isChangeId } from '../../f/fmt';
  import HistorySearch from './HistorySearch.svelte';
  import TimeTravel from './TimeTravel.svelte';
  import SemanticDiff from '../SemanticDiff.svelte';
  import UnifiedDiff from '../UnifiedDiff.svelte';
  import StateChip from '../StateChip.svelte';
  import Glyph from '../Glyph.svelte';

  let { query, sha }: { query: URLSearchParams; sha?: string } = $props();

  const filters = $derived(fromParams(query));
  let entries = $state<HistoryEntry[]>([]);
  let total = $state(0);
  let loading = $state(true);
  let error = $state<string | null>(null);
  let detail = $state<HistoryDetail | null>(null);
  let detailError = $state<string | null>(null);
  let openFiles = $state<Record<string, boolean>>({});

  async function load(f: HistoryQuery) {
    loading = true;
    try {
      const r = await must(api.GET('/history', { params: { query: { ...f, limit: 300 } } }));
      entries = r.entries;
      total = r.total ?? r.entries.length;
      error = null;
    } catch (e) {
      error = errorText(e);
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void load(filters);
  });

  // New merges show up without a reload.
  $effect(() =>
    events.on<{ what: string }>('forge.updated', (d) => {
      if (d.what === 'merge') void load(filters);
    }),
  );

  // Selected entry: the route sha, else ?sel=, else the only match.
  const selSha = $derived(sha ?? query.get('sel') ?? (entries.length === 1 && (filters.q || filters.entity) ? entries[0].sha : undefined));

  // /history/c-2hd7vw works too: a change id resolves to its merge commit.
  async function resolve(s: string): Promise<string> {
    if (!isChangeId(s)) return s;
    const hit = untrack(() => entries.find((e) => e.change === s));
    if (hit) return hit.sha;
    const r = await must(api.GET('/history', { params: { query: { q: s, limit: 50 } } }));
    const m = r.entries.find((e) => e.change === s);
    if (!m) throw new Error(`${s} is not merged into main`);
    return m.sha;
  }

  async function loadDetail(s: string | undefined) {
    detail = null;
    detailError = null;
    openFiles = {};
    if (!s) return;
    try {
      detail = await must(api.GET('/history/{sha}', { params: { path: { sha: await resolve(s) } } }));
    } catch (e) {
      detailError = errorText(e);
    }
  }
  $effect(() => {
    void loadDetail(selSha);
  });

  function search(q: HistoryQuery) {
    const p = new URLSearchParams(toParams(q));
    router.navigate(`/history${p.size ? `?${p}` : ''}`);
  }

  function select(e: HistoryEntry) {
    const p = new URLSearchParams(toParams(filters));
    router.navigate(`/history/${e.sha}${p.size ? `?${p}` : ''}`);
  }

  const by = (e: HistoryEntry) => (e.agent ? `${e.author ?? ''} + agent` : (e.author ?? ''));
  const filterQS = $derived(toParams(filters));
  const filesOf = $derived((detail?.files ?? []).filter((f) => !f.path.startsWith('.pk/changes/')));
</script>

<div class="hb" class:withdetail={!!selSha}>
  <section class="listcol">
    <div class="top">
      <HistorySearch value={filters} onsearch={search} />
      <span class="scope mono muted">main · merge commits</span>
    </div>

    {#if error}<div class="error-box">{error}</div>{/if}

    <div class="tablewrap">
      <table class="table hist">
        <thead>
          <tr><th class="d">Date</th><th class="c">Change</th><th>Title</th><th class="b">By</th><th class="l">Lab</th></tr>
        </thead>
        <tbody>
          {#each entries as e (e.sha)}
            <tr
              class:sel={selSha && (e.sha.startsWith(selSha) || e.change === selSha)}
              class="t-{e.type}"
              onclick={() => select(e)}
              tabindex="0"
              onkeydown={(k) => k.key === 'Enter' && select(e)}
            >
              <td class="d mono" title={dateTime(e.date)}>{shortDate(e.date)}</td>
              <td class="c mono">{e.change ?? short(e.sha)}</td>
              <td class="t">
                <span class="title">{e.title}</span>
                {#if e.type === 'break-glass'}<span class="bg" title={e.breakGlass}><Glyph name="alert" size={12} /> break-glass</span>{/if}
                {#if e.type === 'direct'}<span class="direct">direct</span>{/if}
              </td>
              <td class="b">{by(e)}</td>
              <td class="l">
                {#if e.labRun}<span class="lab r-{e.labRun.result}">{e.labRun.result}</span>{:else if e.type === 'break-glass'}<span class="lab r-fail" title="Break-glass use: no lab, an alert">alert</span>{:else}<span class="muted">—</span>{/if}
              </td>
            </tr>
          {:else}
            {#if !loading}
              <tr><td colspan="5" class="empty">No merge matches these filters.</td></tr>
            {/if}
          {/each}
        </tbody>
      </table>
    </div>
    <p class="muted small foot">{loading ? 'Loading…' : `${entries.length}${total > entries.length ? ` of ${total}` : ''} entries`} · auditors read the same with <code>git log --merges --format='%(trailers)'</code></p>
  </section>

  {#if selSha}
    <aside class="detail">
      {#if detail}
        {@const e = detail.entry}
        <header class="dh">
          <h2>{e.title}</h2>
          {#if e.state}<StateChip state={e.state} />{/if}
          <span class="spacer"></span>
          <a class="btn ghost small" href={href('/history', filterQS)} title="Close"><Glyph name="x" size={14} /></a>
        </header>
        <div class="ids mono muted">
          {#if e.change}
            <a href={href(`/changes/${e.change}`)}>{e.change}</a>
          {/if}
          {#if e.mr}<span>MR {e.mr}</span>{/if}
          <span>merge {e.short}</span>
          <span title={dateTime(e.date)}>{dateTime(e.date)}</span>
        </div>
        {#if e.type === 'break-glass'}
          <div class="bgbox"><Glyph name="alert" size={14} /> Break-glass: <span class="mono">{e.breakGlass}</span></div>
        {/if}

        {#if e.trailers.length}
          <section class="trailers">
            <h3>Trailers</h3>
            <dl>
              {#each e.trailers as t, i (i)}
                <div>
                  <dt>{t.key}:</dt>
                  <dd>
                    {#if t.key === 'Pk-Change' && isChangeId(t.value)}
                      <a href={href(`/changes/${t.value}`)}>{t.value}</a>
                    {:else if t.key === 'Pk-Lab-Run'}
                      {@const parts = t.value.split(' ')}
                      <a href={href(`/labs/runs/${encodeURIComponent(parts[0])}`)}>{parts[0]}</a>
                      {parts[1] ?? ''}
                      {#if parts[2]}<a href={href('/kb/article', { path: parts[2] })}>{parts[2]}</a>{/if}
                    {:else if t.key === 'Pk-Work-Item'}
                      <a href={href('/work', { item: t.value })}>{t.value}</a>
                    {:else}
                      {t.value}
                    {/if}
                  </dd>
                </div>
              {/each}
            </dl>
          </section>
        {/if}

        <section class="box">
          <h3>Semantic diff</h3>
          <SemanticDiff lines={detail.semantic} fileHref={(l) => (l.path ? href('/files', { path: l.path }) : undefined)} />
        </section>

        {#if e.files?.length}
          <TimeTravel sha={e.sha} files={e.files} semantic={detail.semantic} later={entries.filter((x) => x.date > e.date)} />
        {/if}

        {#if detail.rollout?.length}
          <section class="box">
            <h3>Rollout</h3>
            <div class="roll">
              {#each detail.rollout as r (r.cluster)}
                <span class="rc">
                  <span class="mono">{r.cluster}</span>
                  {#if r.ring}<span class="muted small">{r.ring}</span>{/if}
                  <span class="st {r.verified ? 'ok' : r.deployed ? 'dep' : 'no'}">{r.verified ? 'verified' : r.deployed ? 'deployed' : 'not yet'}</span>
                </span>
              {/each}
            </div>
          </section>
        {/if}

        {#if filesOf.length}
          <section class="box">
            <h3>Files <span class="muted">{filesOf.length}</span></h3>
            {#each filesOf as f (f.path)}
              <div class="file">
                <button class="fh" onclick={() => (openFiles[f.path] = !openFiles[f.path])} aria-expanded={!!openFiles[f.path]}>
                  <span class="fs fs-{f.status}">{f.status[0].toUpperCase()}</span>
                  <span class="mono">{f.path}</span>
                </button>
                {#if openFiles[f.path]}<UnifiedDiff diff={f.unified} />{/if}
              </div>
            {/each}
          </section>
        {/if}

        {#if detail.message}
          <details class="msg">
            <summary class="small muted">Merge commit message</summary>
            <pre>{detail.message}</pre>
          </details>
        {/if}
      {:else if detailError}
        <div class="error-box">{detailError}</div>
      {:else}
        <p class="muted">Loading…</p>
      {/if}
    </aside>
  {/if}
</div>

<style>
  .hb {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    height: 100%;
    min-height: 0;
  }
  .hb.withdetail {
    grid-template-columns: minmax(0, 1fr) minmax(420px, 40%);
  }
  .listcol {
    display: grid;
    grid-template-rows: auto auto minmax(0, 1fr) auto;
    gap: 10px;
    padding: 14px 0 10px;
    min-height: 0;
    background: var(--panel);
  }
  .top {
    display: flex;
    gap: 14px;
    align-items: flex-start;
    padding: 0 16px;
  }
  .scope {
    font-size: 13px;
    padding-top: 9px;
    white-space: nowrap;
  }
  .error-box {
    margin: 0 16px;
  }
  .tablewrap {
    overflow: auto;
    min-height: 0;
  }
  .hist th {
    position: sticky;
    top: 0;
    background: var(--panel);
    z-index: 1;
  }
  .hist th:first-child,
  .hist td:first-child {
    padding-left: 16px;
  }
  .hist tr {
    cursor: pointer;
  }
  .hist tbody tr:hover td {
    background: var(--panel-2);
  }
  .hist tr.sel td {
    background: var(--accent-soft) !important;
  }
  .hist tr.sel td:first-child {
    box-shadow: inset 3px 0 0 var(--accent-fill);
  }
  .hist td {
    vertical-align: middle;
    padding-top: 9px;
    padding-bottom: 9px;
  }
  .d {
    width: 84px;
    white-space: nowrap;
    color: var(--muted);
  }
  .c {
    width: 100px;
    color: var(--accent);
  }
  .b {
    width: 140px;
  }
  .l {
    width: 70px;
  }
  .title {
    margin-right: 6px;
  }
  .t-direct .title {
    color: var(--muted);
  }
  .bg {
    display: inline-flex;
    align-items: center;
    gap: 3px;
    font-size: 11px;
    font-weight: 600;
    padding: 0 6px;
    border-radius: 4px;
    background: var(--bad-soft);
    color: var(--bad);
  }
  .direct {
    font-size: 11px;
    padding: 0 6px;
    border-radius: 4px;
    background: var(--line);
    color: var(--muted);
  }
  .lab {
    font-size: 11.5px;
    font-weight: 600;
    padding: 1px 8px;
    border-radius: 4px;
  }
  .r-pass {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .r-fail {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .foot {
    margin: 0;
    padding: 0 16px;
  }
  .detail {
    border-left: 1px solid var(--line);
    padding: 14px 18px 28px;
    overflow-y: auto;
    display: grid;
    gap: 12px;
    align-content: start;
    min-width: 0;
  }
  .dh {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .dh h2 {
    font-size: 19px;
  }
  .ids {
    display: flex;
    gap: 14px;
    font-size: 12.5px;
    margin-top: -6px;
    flex-wrap: wrap;
  }
  .bgbox {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    border-radius: var(--radius-s);
    background: var(--bad-soft);
    color: var(--bad);
    font-size: 13px;
  }
  .trailers {
    border-left: 4px solid var(--accent-fill);
    background: var(--code-bg);
    padding: 10px 14px;
    border-radius: 0 var(--radius-s) var(--radius-s) 0;
  }
  .trailers h3 {
    color: var(--accent);
    margin-bottom: 6px;
  }
  dl {
    margin: 0;
    font: 12.5px/1.7 var(--mono);
  }
  dl div {
    display: flex;
    gap: 6px;
  }
  dt {
    color: var(--info);
    white-space: nowrap;
  }
  dd {
    margin: 0;
    word-break: break-word;
  }
  .box {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px 14px;
    display: grid;
    gap: 6px;
    background: var(--panel);
  }
  .roll {
    display: flex;
    flex-wrap: wrap;
    gap: 12px;
  }
  .rc {
    display: inline-flex;
    gap: 6px;
    align-items: baseline;
  }
  .st {
    font-size: 11.5px;
    font-weight: 600;
    padding: 0 7px;
    border-radius: 4px;
  }
  .st.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .st.dep {
    background: var(--info-soft);
    color: var(--info);
  }
  .st.no {
    background: var(--line);
    color: var(--muted);
  }
  .file {
    display: grid;
    gap: 4px;
  }
  .fh {
    display: flex;
    gap: 8px;
    align-items: center;
    border: 0;
    background: none;
    padding: 2px 0;
    text-align: left;
    font-size: 12.5px;
  }
  .fh:hover .mono {
    color: var(--accent);
  }
  .fs {
    display: inline-grid;
    place-items: center;
    width: 16px;
    height: 16px;
    border-radius: 3px;
    font: 700 10px var(--mono);
    background: var(--warn-soft);
    color: var(--warn);
  }
  .fs-added {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .fs-deleted {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .msg pre {
    margin-top: 6px;
    font-size: 12px;
    white-space: pre-wrap;
  }
  .empty {
    text-align: center;
    color: var(--muted);
    padding: 24px;
  }
  @media (max-width: 640px) {
    .hb.withdetail {
      grid-template-columns: minmax(0, 1fr);
    }
    .scope,
    .hist .b,
    .hist .l {
      display: none;
    }
  }
</style>
