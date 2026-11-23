<script lang="ts">
  // Rack elevation with the selected device next to it (?sel=cn-035).
  import type { Params } from '../../lib/router.svelte';
  import { router, href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { Entity, Rack, RackOccupant } from '../../lib/api/ftypes';
  import { work } from '../../lib/stores/context.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import RackElevation from '../../lib/components/assets/RackElevation.svelte';
  import EntityView from '../../lib/components/assets/EntityView.svelte';
  import RefSwitch from '../../lib/components/assets/RefSwitch.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';
  import Icon from '../../lib/components/Icon.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const ref = $derived(work.refFor(query));
  const refQ = $derived(ref === 'main' ? {} : { ref });
  const sel = $derived(query.get('sel') ?? undefined);

  let rack = $state<Rack | null>(null);
  let entity = $state<Entity | null>(null);
  let error = $state<string | null>(null);
  let entError = $state<string | null>(null);

  async function loadRack() {
    try {
      rack = await must(api.GET('/racks/{name}', { params: { path: { name: params.name }, query: refQ } }));
      error = null;
    } catch (e) {
      error = errorText(e);
    }
  }

  const selOcc = $derived(rack?.occupants.find((o) => o.name === sel));

  // The selected device is not on main but in the change you work on: an
  // expected state, shown as a note, not as an error.
  let newInChange = $state<string | null>(null);
  // Only the newest load counts (switching main/change races the rack load).
  let seq = 0;

  async function loadEntity(o: RackOccupant | undefined) {
    const n = ++seq;
    newInChange = null;
    if (!o) {
      entity = null;
      entError = null;
      const cid = work.changeId;
      if (sel && rack && ref === 'main' && cid) {
        const there = await api.GET('/racks/{name}', { params: { path: { name: params.name }, query: { ref: cid } } });
        if (n === seq && there.data?.occupants.some((x) => x.name === sel)) newInChange = cid;
      }
      return;
    }
    try {
      const e = await must(
        api.GET('/entities/{kind}/{name}', { params: { path: { kind: o.kind, name: o.name }, query: refQ } }),
      );
      if (n !== seq) return;
      entity = e;
      entError = null;
    } catch (e) {
      if (n !== seq) return;
      entity = null;
      entError = errorText(e);
    }
  }

  $effect(() => {
    void ref;
    void loadRack();
  });
  $effect(() => {
    void loadEntity(selOcc);
  });

  // A save in the change re-renders: reload the view.
  $effect(() =>
    events.on<{ changeId: string }>('render.updated', (d) => {
      if (d.changeId === ref) {
        void loadRack();
        void loadEntity(selOcc);
      }
    }),
  );

  function select(o: RackOccupant) {
    const q = new URLSearchParams(router.query);
    q.set('sel', o.name);
    router.navigate(`${router.path}?${q}`);
  }

  const problems = $derived(rack?.occupants.filter((o) => o.problems?.length) ?? []);
  const highlighted = $derived(rack?.occupants.filter((o) => o.highlight && o.highlight !== 'none') ?? []);
  const freeRanges = $derived.by(() => {
    const f = [...(rack?.free ?? [])].sort((a, b) => a - b);
    const out: string[] = [];
    for (let i = 0; i < f.length; ) {
      let j = i;
      while (j + 1 < f.length && f[j + 1] === f[j] + 1) j++;
      out.push(i === j ? `U${f[i]}` : `U${f[i]}–${f[j]}`);
      i = j + 1;
    }
    return out;
  });
</script>

<div class="rackpage">
  <aside class="left">
    <div class="rhead">
      <a class="mono name" href={href('/assets', { ref: ref === 'main' ? undefined : ref })} title="All assets">{params.name}</a>
      <span class="muted small">{rack ? `${rack.units}U${rack.row ? ` · row ${rack.row}` : ''}` : ''}</span>
    </div>
    {#if rack}
      <RackElevation {rack} selected={sel} onselect={select} unitPx={15} />
    {:else if error}
      <div class="error-box">{error}</div>
    {:else}
      <p class="muted small">Loading…</p>
    {/if}
  </aside>

  <section class="right">
    <div class="bar">
      <RefSwitch value={ref} changeId={work.changeId} />
      {#if ref !== 'main'}<span class="muted small">Changes in {ref} are highlighted against main.</span>{/if}
    </div>

    {#if sel && entity}
      <EntityView {entity} />
    {:else if sel && entError}
      <div class="error-box">{entError}</div>
    {:else if sel && newInChange}
      <div class="callout">
        <Icon name="info" size={16} />
        <span>
          {sel} is new in this change; it is not on main yet.
          <a href={href(`/assets/racks/${encodeURIComponent(params.name)}`, { sel, ref: newInChange, pinref: '1' })}>Show it in {newInChange}</a>
        </span>
      </div>
    {:else if rack}
      <div class="summary">
        <header>
          <span class="ico"><Glyph name="board" size={24} /></span>
          <h1 class="mono">{rack.name}</h1>
          <span class="kv">Rack</span>
          <span class="spacer"></span>
          {#if rack.path}<a class="path" href={href('/files', { path: rack.path })}>{rack.path}</a>{/if}
        </header>
        {#if rack.location}<p class="muted">{rack.location}</p>{/if}

        {#if highlighted.length}
          <div class="card hl">
            <h3>In <span class="keep">{ref}</span></h3>
            {#each highlighted as o (o.name)}
              <button class="row-btn" onclick={() => select(o)}>
                <span class="tag h-{o.highlight}">{o.highlight}</span>
                <span class="mono">{o.name}</span>
                <span class="muted small">U{o.unit}{o.size > 1 ? `–${o.unit + o.size - 1}` : ''} · {o.role ?? o.kind}</span>
              </button>
            {/each}
          </div>
        {/if}

        {#if problems.length || rack.problems?.length}
          <div class="card bad">
            <h3>Problems</h3>
            {#each problems as o (o.name)}
              {#each o.problems ?? [] as p, i (i)}
                <button class="row-btn" onclick={() => select(o)}><span class="mono">{o.name}</span> <span>{p}</span></button>
              {/each}
            {/each}
            {#each rack.problems ?? [] as p, i (i)}<div class="small">{p.message}</div>{/each}
          </div>
        {/if}

        <div class="card">
          <h3>Devices</h3>
          <table class="table">
            <thead><tr><th>Unit</th><th>Name</th><th>Kind</th><th>Role</th><th>Cluster</th></tr></thead>
            <tbody>
              {#each [...rack.occupants].sort((a, b) => b.unit - a.unit) as o (o.kind + o.name)}
                <tr>
                  <td class="mono">U{o.unit}{o.size > 1 ? `–${o.unit + o.size - 1}` : ''}</td>
                  <td><button class="link mono" onclick={() => select(o)}>{o.name}</button></td>
                  <td>{o.kind}</td>
                  <td>{o.role ?? o.category ?? '—'}</td>
                  <td>{o.cluster ?? '—'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
          <p class="muted small free">Free: {freeRanges.join(', ') || 'none'}</p>
        </div>
        <p class="muted small">Select a device in the rack to see its intent, hardware facts and cables.</p>
      </div>
    {/if}
  </section>
</div>

<style>
  .rackpage {
    display: grid;
    grid-template-columns: 300px minmax(0, 1fr);
    min-height: 100%;
  }
  .left {
    border-right: 1px solid var(--line);
    background: var(--panel);
    padding: 14px 16px;
    display: grid;
    align-content: start;
    gap: 10px;
  }
  .rhead {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
  }
  .name {
    font-size: 15px;
    font-weight: 600;
    color: var(--ink);
    text-decoration: none;
  }
  .right {
    padding: 14px 20px 32px;
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    align-content: start;
    min-width: 0;
  }
  .bar {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
    align-items: center;
  }
  .summary {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .summary header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
  }
  .summary p {
    margin: 0;
  }
  .ico {
    color: var(--info);
    display: inline-flex;
  }
  h1 {
    font-weight: 600;
    white-space: nowrap;
  }
  .kv {
    font: 600 12px/1 var(--mono);
    background: var(--gsi-navy);
    color: #fff;
    padding: 5px 8px;
    border-radius: 4px;
  }
  .path {
    color: var(--muted);
    font-size: 13px;
    text-decoration: none;
    overflow-wrap: anywhere;
  }
  .card {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 12px 16px;
    display: grid;
    gap: 4px;
  }
  .card h3 {
    margin-bottom: 4px;
  }
  .card.bad {
    border-color: color-mix(in srgb, var(--bad) 40%, transparent);
  }
  .row-btn {
    display: flex;
    gap: 10px;
    align-items: baseline;
    border: 0;
    background: none;
    padding: 3px 0;
    text-align: left;
    font-size: 13px;
  }
  .row-btn:hover .mono {
    color: var(--accent);
  }
  .tag {
    font-size: 11px;
    font-weight: 600;
    padding: 0 6px;
    border-radius: 4px;
    background: var(--ok-soft);
    color: var(--ok);
  }
  .tag.h-changed {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .tag.h-deleted,
  .tag.h-conflict {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .link {
    border: 0;
    background: none;
    padding: 0;
    color: var(--accent);
  }
  .free {
    margin: 6px 0 0;
  }
  @media (max-width: 900px) {
    .rackpage {
      grid-template-columns: minmax(0, 1fr);
    }
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
