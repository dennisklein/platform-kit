<script lang="ts">
  // One entity (/assets/Node/cn-035). Devices in a rack show the rack next to it.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { api, must, errorText, ApiError } from '../../lib/api/client';
  import type { Entity, Rack, RackOccupant } from '../../lib/api/ftypes';
  import { work } from '../../lib/stores/context.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import RackElevation from '../../lib/components/assets/RackElevation.svelte';
  import EntityView from '../../lib/components/assets/EntityView.svelte';
  import RefSwitch from '../../lib/components/assets/RefSwitch.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  // The change the user came from may not hold this entity (it is on main
  // only, or merged after the change branched): then show main.
  let fellBack = $state<string | null>(null);
  const wanted = $derived(work.refFor(query));
  const ref = $derived(fellBack === wanted ? 'main' : wanted);
  const refQ = $derived(ref === 'main' ? {} : { ref });
  // Third-party objects can share kind and name across clusters
  // (HelmRelease cilium): ?cluster= or ?path= picks one.
  const pickCluster = $derived(query.get('cluster') ?? undefined);
  const pickPath = $derived(query.get('path') ?? undefined);

  let entity = $state<Entity | null>(null);
  let rack = $state<Rack | null>(null);
  let error = $state<string | null>(null);

  const rackName = $derived(entity?.references.find((r) => r.kind === 'Rack' && r.exists)?.name);

  async function load() {
    const r = ref;
    try {
      entity = await must(
        api.GET('/entities/{kind}/{name}', {
          params: { path: { kind: params.kind, name: params.name }, query: { ...refQ, cluster: pickCluster, path: pickPath } },
        }),
      );
      error = null;
    } catch (e) {
      if (e instanceof ApiError && e.status === 404 && r !== 'main' && !query.has('ref')) {
        fellBack = r;
        return;
      }
      entity = null;
      error = errorText(e);
    }
  }

  async function loadRack(name: string | undefined) {
    if (!name) {
      rack = null;
      return;
    }
    try {
      rack = await must(api.GET('/racks/{name}', { params: { path: { name }, query: refQ } }));
    } catch {
      rack = null;
    }
  }

  $effect(() => {
    void params.kind;
    void params.name;
    fellBack = null;
  });
  $effect(() => {
    void ref;
    void params.kind;
    void params.name;
    void pickCluster;
    void pickPath;
    void load();
  });

  const altLink = (a: { cluster?: string; path: string }) =>
    href(`/assets/${encodeURIComponent(params.kind)}/${encodeURIComponent(params.name)}`, {
      cluster: a.cluster || undefined,
      path: a.cluster ? undefined : a.path,
      ref: query.get('ref') ?? undefined,
      pinref: query.get('pinref') ?? undefined,
    });
  $effect(() => {
    void loadRack(rackName);
  });
  $effect(() =>
    events.on<{ changeId: string }>('render.updated', (d) => {
      if (d.changeId === ref) {
        void load();
        void loadRack(rackName);
      }
    }),
  );

  const linkOf = (o: RackOccupant) =>
    o.kind === 'Rack'
      ? href(`/assets/racks/${encodeURIComponent(o.name)}`)
      : href(`/assets/${o.kind}/${encodeURIComponent(o.name)}`, { ref: query.get('ref') ?? undefined, pinref: query.get('pinref') ?? undefined });
</script>

<div class="entpage" class:withrack={!!rack}>
  {#if rack}
    <aside class="left">
      <div class="rhead">
        <a class="mono name" href={href(`/assets/racks/${encodeURIComponent(rack.name)}`, { ref: ref === 'main' ? undefined : ref })}>{rack.name}</a>
        <span class="muted small">{rack.units}U{rack.row ? ` · row ${rack.row}` : ''}</span>
      </div>
      <RackElevation {rack} selected={params.name} {linkOf} />
    </aside>
  {/if}
  <section class="right">
    <div class="bar">
      <a class="crumb" href={href('/assets')}>Assets</a>
      <span class="muted">/</span>
      <span class="muted">{params.kind}</span>
      <span class="spacer"></span>
      <RefSwitch value={ref} changeId={work.changeId} />
    </div>
    {#if fellBack && entity}
      <p class="small muted note">{params.name} is not in {fellBack}; this is main.</p>
    {/if}
    {#if entity?.alternatives?.length}
      <p class="small muted note alts">
        {entity.kind} {entity.name} exists in {entity.alternatives.length + 1} places. This is
        <b>{entity.cluster || entity.path}</b>; also in
        {#each entity.alternatives as a, i (a.path)}{i ? ', ' : ''}<a href={altLink(a)}>{a.cluster || a.path}</a>{/each}.
      </p>
    {/if}
    {#if entity}
      <EntityView {entity} />
    {:else if error}
      <div class="error-box">{error}</div>
    {:else}
      <p class="muted">Loading…</p>
    {/if}
  </section>
</div>

<style>
  .entpage {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    min-height: 100%;
  }
  .entpage.withrack {
    grid-template-columns: 300px minmax(0, 1fr);
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
    gap: 12px;
    align-content: start;
    min-width: 0;
  }
  .bar {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
  }
  .crumb {
    text-decoration: none;
  }
  .note {
    margin: 0;
  }
  @media (max-width: 900px) {
    .entpage.withrack {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
