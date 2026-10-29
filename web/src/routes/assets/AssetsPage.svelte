<script lang="ts">
  import { untrack } from 'svelte';
  // Assets: racks, nodes, switches, JBODs, clusters and the cable plan, from
  // main or from the change you work on.
  import type { Params } from '../../lib/router.svelte';
  import { router, href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { EntitySummary, Rack } from '../../lib/api/ftypes';
  import { work } from '../../lib/stores/context.svelte';
  import RefSwitch from '../../lib/components/assets/RefSwitch.svelte';
  import RackElevation from '../../lib/components/assets/RackElevation.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';
  import PageHeader from '../../lib/components/PageHeader.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const ref = $derived(work.refFor(query));
  let filter = $state(untrack(() => query.get('q')) ?? '');
  let list = $state<EntitySummary[]>([]);
  let racks = $state<Rack[]>([]);
  let error = $state<string | null>(null);
  let loading = $state(true);

  const ASSET_KINDS = ['Rack', 'Node', 'Switch', 'Jbod', 'Cluster', 'CablePlan'];

  async function load(r: string) {
    loading = true;
    try {
      const refQ = r === 'main' ? {} : { ref: r };
      const all = await must(api.GET('/entities', { params: { query: refQ } }));
      list = all;
      const names = all.filter((e) => e.kind === 'Rack').map((e) => e.name);
      racks = await Promise.all(
        names.map((n) => must(api.GET('/racks/{name}', { params: { path: { name: n }, query: refQ } }))),
      );
      error = null;
    } catch (e) {
      error = errorText(e);
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void load(ref);
  });

  const matches = (e: EntitySummary) => {
    const f = filter.trim().toLowerCase();
    if (!f) return true;
    return [e.name, e.kind, e.cluster, e.role, e.title, e.path, ...Object.entries(e.labels ?? {}).map(([k, v]) => `${k}=${v}`)]
      .filter(Boolean)
      .some((s) => s!.toLowerCase().includes(f));
  };

  const byKind = $derived(
    ASSET_KINDS.map((k) => [k, list.filter((e) => e.kind === k && matches(e))] as const).filter(([, xs]) => xs.length),
  );
  const others = $derived(list.filter((e) => !ASSET_KINDS.includes(e.kind) && matches(e)));
  const changed = $derived(
    racks.flatMap((r) => r.occupants.filter((o) => o.highlight && o.highlight !== 'none').map((o) => ({ ...o, rack: r.name }))),
  );

  const KIND_LABEL: Record<string, string> = {
    Node: 'Nodes',
    Switch: 'Switches',
    Jbod: 'JBODs',
    Cluster: 'Clusters',
    CablePlan: 'Cable plan',
    Rack: 'Racks',
  };
  // Kind and name pairs that exist more than once (HelmRelease cilium in
  // each cluster): their links pick the file.
  const shared = $derived.by(() => {
    const n = new Map<string, number>();
    for (const e of list) n.set(`${e.kind}/${e.name}`, (n.get(`${e.kind}/${e.name}`) ?? 0) + 1);
    return new Set([...n].filter(([, c]) => c > 1).map(([k]) => k));
  });
  const isShared = (e: { kind: string; name: string }) => shared.has(`${e.kind}/${e.name}`);
  const entityHref = (e: { kind: string; name: string; path?: string }) =>
    e.kind === 'Rack'
      ? href(`/assets/racks/${encodeURIComponent(e.name)}`, { ref: ref === 'main' ? undefined : ref })
      : href(`/assets/${e.kind}/${encodeURIComponent(e.name)}`, {
          ref: ref === 'main' ? undefined : ref,
          path: isShared(e) ? e.path : undefined,
        });
  const occupantsOf = (r: Rack) => r.occupants.length;
  // Kinds without an entity view (named by their file, or Talos config):
  // their links open the file.
  const FILE_KINDS = new Set(['TalosConfig', 'Policy', 'MilestoneList']);
  const otherHref = (e: EntitySummary) =>
    FILE_KINDS.has(e.kind) || (e.path && e.name === e.path)
      ? href(work.changeId && ref !== 'main' ? `/changes/${ref}/files` : '/files', { path: e.path })
      : entityHref(e);
</script>

<div class="page">
  <PageHeader title="Assets">
    {#snippet actions()}
      <RefSwitch value={ref} changeId={work.changeId} />
      <label class="search">
        <Glyph name="search" size={15} />
        <input class="input" placeholder="Filter: cn-03, defiant, worker, firmware=2.4" bind:value={filter} />
      </label>
    {/snippet}
  </PageHeader>

  {#if error}<div class="error-box">{error}</div>{/if}

  {#if ref !== 'main' && changed.length}
    <div class="inchange">
      <Glyph name="branch" size={14} />
      <span>In <a class="mono" href={href(`/changes/${ref}`)}>{ref}</a>:</span>
      {#each changed as o (o.name)}
        <a class="hl h-{o.highlight}" href={entityHref(o)}>{o.highlight === 'added' ? '+' : o.highlight === 'deleted' ? '−' : '~'} {o.name} in {o.rack} U{o.unit}</a>
      {/each}
    </div>
  {/if}

  <div class="overview" class:split={racks.length > 0}>
  {#if racks.length}
    <section class="racks">
      {#each racks as r (r.name)}
        <a class="rackcard" href={entityHref({ kind: 'Rack', name: r.name })}>
          <div class="rc-head">
            <strong class="mono">{r.name}</strong>
            <span class="muted small">{r.units}U{r.row ? ` · row ${r.row}` : ''}</span>
          </div>
          <div class="mini"><RackElevation rack={r} unitPx={12} compact /></div>
          <div class="muted small">{occupantsOf(r)} devices · {r.free.length} U free</div>
          {#if r.location}<div class="muted small loc">{r.location}</div>{/if}
        </a>
      {/each}
    </section>
  {/if}

  <div class="tables">
  {#each byKind.filter(([k]) => k !== 'Rack') as [kind, xs] (kind)}
    <section class="section tsec">
      <h3>{KIND_LABEL[kind] ?? kind} <span class="n">{xs.length}</span></h3>
      <table class="table">
        <thead>
          <tr>
            <th>Name</th>
            {#if kind === 'Node' || kind === 'Cluster'}<th>Role</th>{/if}
            {#if kind === 'Node'}<th>Cluster</th><th>Labels</th>{/if}
            <th>Path</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {#each xs as e (e.name)}
            <tr>
              <td><a class="mono" href={entityHref(e)}>{e.name}</a></td>
              {#if kind === 'Node' || kind === 'Cluster'}<td>{e.role ?? '—'}</td>{/if}
              {#if kind === 'Node'}
                <td>{e.cluster ?? '—'}</td>
                <td class="mono small">{Object.entries(e.labels ?? {}).map(([k, v]) => `${k}=${v}`).join(' ') || '—'}</td>
              {/if}
              <td class="mono small muted">{e.path}</td>
              <td><span class="st st-{e.status ?? 'ok'}">{e.status ?? 'ok'}</span></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/each}

  {#if others.length}
    <details class="section">
      <summary><h3>Other kinds <span class="n">{others.length}</span></h3></summary>
      <div class="others">
        {#each others as e (e.path + e.kind + e.name)}
          <a href={otherHref(e)} title={e.path}><span class="muted small">{e.kind}</span> <span class="mono">{e.name.split('/').pop()}</span>{#if isShared(e) && e.cluster}<span class="muted small"> · {e.cluster}</span>{/if}</a>
        {/each}
      </div>
    </details>
  {/if}
  </div>
  </div>

  {#if loading && !list.length}<p class="empty">Loading…</p>{/if}
  {#if !loading && !error && !byKind.length && !others.length}<p class="empty">No assets match “{filter}”.</p>{/if}
</div>

<style>
  /* One column that may shrink below its content (phone width). */
  .page {
    grid-template-columns: minmax(0, 1fr);
  }
  .page {
    min-width: 0;
    container-type: inline-size;
  }
  /* Racks on the left, the tables beside them when the page is wide; one
     column below that. */
  .overview,
  .tables {
    display: grid;
    gap: 14px;
    align-content: start;
    min-width: 0;
  }
  @container (min-width: 1100px) {
    .overview.split {
      grid-template-columns: auto minmax(0, 1fr);
      align-items: start;
      gap: 20px;
    }
    .overview.split .racks {
      max-width: 632px;
    }
  }
  .search {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--muted);
  }
  .search .input {
    width: 320px;
    max-width: calc(100vw - 120px);
  }
  .tsec {
    min-width: 0;
    overflow-x: auto;
  }
  /* Racks as small elevations with names, side by side (slide 9). */
  .racks {
    display: flex;
    flex-wrap: wrap;
    gap: 16px;
  }
  .rackcard {
    display: grid;
    gap: 8px;
    width: 300px;
    max-width: 100%;
    padding: 12px 14px 14px;
    border: 1px solid var(--line);
    border-radius: var(--radius);
    background: var(--panel);
    color: var(--ink);
    text-decoration: none;
  }
  .rackcard:hover {
    border-color: var(--accent-fill);
  }
  .rc-head {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
  }
  .mini :global(.occ) {
    pointer-events: none;
  }
  .loc {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .n {
    font-weight: 400;
    color: var(--muted);
    margin-left: 4px;
  }
  .section h3 {
    margin-bottom: 6px;
  }
  .st {
    font-size: 11.5px;
    font-weight: 600;
    padding: 0 7px;
    border-radius: 4px;
    background: var(--ok-soft);
    color: var(--ok);
  }
  .st-warning {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .st-error {
    background: var(--bad-soft);
    color: var(--bad);
  }
  details summary {
    cursor: pointer;
    list-style: none;
  }
  details summary h3 {
    display: inline;
  }
  .others {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
    gap: 4px 16px;
    margin-top: 8px;
  }
  .others a {
    color: var(--ink);
    text-decoration: none;
    font-size: 13px;
  }
  .others a:hover .mono {
    color: var(--accent);
  }
  .inchange {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
    padding: 8px 12px;
    border-radius: var(--radius-s);
    background: var(--ok-soft);
    font-size: 13px;
  }
  .hl {
    font: 500 12.5px var(--mono);
    text-decoration: none;
    color: var(--ok);
  }
  .hl.h-changed {
    color: var(--warn);
  }
  .hl.h-deleted,
  .hl.h-conflict {
    color: var(--bad);
  }
  /* Wide tables scroll on their own at phone width. */
  @media (max-width: 700px) {
    .table {
      display: block;
      overflow-x: auto;
    }
    .table :is(td, th) {
      white-space: nowrap;
    }
  }
</style>
