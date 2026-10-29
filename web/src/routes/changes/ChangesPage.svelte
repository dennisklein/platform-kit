<script lang="ts">
  // Changes without a selection: the bench at a glance (open changes,
  // reviews, labs, clusters), the clusters Flux applies main to, and the
  // recent merges of main. Start a change here, or pick one on the left.
  import type { Params } from '../../lib/router.svelte';
  import { href, seg } from '../../lib/router.svelte';
  import { api, must } from '../../lib/api/client';
  import type { ChangeSummary } from '../../lib/api/types';
  import type { Cluster, RingStatus } from '../../lib/api/ftypes';
  import { changes } from '../../lib/stores/changes.svelte';
  import { events } from '../../lib/stores/events.svelte';
  import { ui } from '../../lib/stores/ui.svelte';
  import { shortSha, timeAgo, plural } from '../../lib/change/format';
  import { changeCounts, clusterHealth } from '../../lib/f/clusters';
  import StateChip from '../../lib/components/StateChip.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { query }: { params: Params; query: URLSearchParams } = $props();

  // null: not loaded yet (skeleton rows, not "No merges yet").
  let recent = $state<ChangeSummary[] | null>(null);
  $effect(() => {
    void changes.main?.sha;
    void (async () => {
      try {
        const r = await must(api.GET('/changes', { params: { query: { scope: 'all' } } }));
        recent = r.changes
          .filter((c) => ['merged', 'deployed', 'verified'].includes(c.state))
          .sort((a, b) => (b.mergedAt ?? '').localeCompare(a.mergedAt ?? ''))
          .slice(0, 8);
      } catch {
        recent = [];
      }
    })();
  });

  let clusters = $state<Cluster[] | null>(null);
  let rings = $state<RingStatus[]>([]);
  async function loadClusters() {
    try {
      const [c, r] = await Promise.all([must(api.GET('/clusters')), must(api.GET('/rings')).catch(() => [] as RingStatus[])]);
      clusters = c;
      rings = r;
    } catch {
      clusters = clusters ?? [];
    }
  }
  $effect(() => {
    void loadClusters();
    let t: ReturnType<typeof setTimeout> | undefined;
    const later = () => {
      clearTimeout(t);
      t = setTimeout(() => void loadClusters(), 300);
    };
    const offs = (['flux.updated', 'forge.updated'] as const).map((e) => events.on(e, later));
    return () => {
      clearTimeout(t);
      offs.forEach((o) => o());
    };
  });

  $effect(() => {
    if (query.get('new') === '1') ui.newChangeOpen = true;
  });

  const n = $derived(changeCounts(changes.list));
  const ready = $derived(changes.loaded);
  const firstReview = $derived(changes.list.find((c) => c.group === 'review'));
  const firstFailed = $derived(changes.list.find((c) => c.labState === 'fail'));
  const ringOrder = $derived(rings.map((r) => r.ring));
  const sorted = $derived(
    [...(clusters ?? [])].sort((a, b) => {
      const ra = ringOrder.indexOf(a.ring);
      const rb = ringOrder.indexOf(b.ring);
      return (ra < 0 ? 99 : ra) - (rb < 0 ? 99 : rb) || a.id.localeCompare(b.id);
    }),
  );
  const healthy = $derived(sorted.filter((c) => clusterHealth(c).tone === 'ok').length);
  const clusterTone = $derived(
    !sorted.length ? 'idle' : sorted.some((c) => clusterHealth(c).tone === 'bad') ? 'bad' : healthy === sorted.length ? 'ok' : 'warn',
  );
  const labTone = $derived(n.labFailed ? 'bad' : n.labRunning ? 'warn' : 'ok');
</script>

<div class="page">
  <header class="head">
    <div class="titles">
      <h1>Changes</h1>
      <p class="lead">
        {#if ready}
          {n.open} open: {n.mine} mine, {n.review} waiting for my review. Pick one on the left, or start a new change. The
          bench creates the worktree, the branch and the metadata file.
        {:else if changes.error}
          The change list did not load: {changes.error}
        {:else}
          <span class="skel line" aria-label="Loading changes"></span>
        {/if}
      </p>
    </div>
    <button class="btn dark big" onclick={() => (ui.newChangeOpen = true)}><Icon name="plus" size={16} /> New change</button>
  </header>

  <section class="tiles" aria-label="At a glance">
    <div class="tile t-ink">
      <span class="eyebrow">Open changes</span>
      <span class="num">{ready ? n.open : '–'}</span>
      <span class="sub">{ready ? `${n.mine} mine · ${n.others} by others` : ' '}</span>
    </div>
    <a class="tile t-{n.review ? 'info' : 'idle'}" href={href(firstReview ? `/changes/${seg(firstReview.id)}` : '/changes')}>
      <span class="eyebrow">Waiting for my review</span>
      <span class="num">{ready ? n.review : '–'}</span>
      <span class="sub" title={firstReview?.title}>{firstReview ? firstReview.title : ready ? 'Nothing to review' : ' '}</span>
    </a>
    <a class="tile t-{ready ? labTone : 'idle'}" href={href(firstFailed ? `/changes/${seg(firstFailed.id)}` : '/labs')}>
      <span class="eyebrow">Change labs</span>
      <span class="num">
        {#if !ready}–{:else if n.labFailed}{n.labFailed} failed{:else if n.labRunning}{n.labRunning} running{:else}all green{/if}
      </span>
      <span class="sub" title={firstFailed?.title}>
        {#if firstFailed}{firstFailed.id} · {firstFailed.title}{:else if ready}{n.labRunning} running{:else}&nbsp;{/if}
      </span>
    </a>
    <a class="tile t-{clusterTone}" href={href('/clusters')}>
      <span class="eyebrow">Clusters</span>
      <span class="num">{clusters ? `${healthy}/${sorted.length} healthy` : '–'}</span>
      <span class="sub">
        {#if changes.main}main <span class="mono">{shortSha(changes.main.sha)}</span>{#if changes.main.change}{' · '}{changes.main.change}{/if}{:else}&nbsp;{/if}
      </span>
    </a>
  </section>

  {#if sorted.length}
    <section class="card" aria-label="Clusters">
      <div class="sh">
        <h3>Clusters</h3>
        <span class="muted small">Flux applies main{rings.some((r) => r.following && r.following !== 'main') ? ' through ring tags' : ''}; a change is verified when health checks pass.</span>
        <span class="spacer"></span>
        <a class="small" href={href('/clusters')}>Open clusters</a>
      </div>
      <div class="ctiles">
        {#each sorted as c (c.id)}
          {@const h = clusterHealth(c)}
          <a class="ctile h-{h.tone}" href={href('/clusters', { cluster: c.id })}>
            <div class="ch">
              <Glyph name="hex" size={16} />
              <span class="cname">{c.id}</span>
              <span class="spacer"></span>
              <span class="pill {h.tone === 'muted' ? '' : h.tone}"><span class="dot"></span>{h.label}</span>
            </div>
            <div class="cmeta">
              {[`ring ${c.ring}`, c.role, c.objects != null ? plural(c.objects, 'object') : ''].filter(Boolean).join(' · ')}
            </div>
            <div class="crev">
              {#if c.applied}
                <span class="mono">{shortSha(c.applied.sha)}</span>
                {#if c.applied.change}<span class="mono chg">{c.applied.change}</span>{/if}
                {#if c.applied.at}<span class="muted">{timeAgo(c.applied.at)}</span>{/if}
              {:else}
                <span class="muted">{c.reachable === false ? 'status unknown' : 'nothing applied yet'}</span>
              {/if}
            </div>
          </a>
        {/each}
      </div>
    </section>
  {/if}

  <section class="card">
    <h3>Recently merged on main</h3>
    <ul class="merged">
      {#if recent === null}
        {#each [0, 1, 2] as i (i)}<li class="skrow" aria-hidden="true"><span class="skel"></span></li>{/each}
      {:else}
        {#each recent as c (c.id)}
          <li>
            <a class="mono" href={href(`/changes/${seg(c.id)}`)}>{c.id}</a>
            <span class="t">{c.title}</span>
            <StateChip state={c.state} />
            <span class="muted small">{c.author} · {timeAgo(c.mergedAt)}</span>
            <span class="mono muted small">{shortSha(c.mergeSha)}</span>
          </li>
        {:else}
          <li class="none muted small">No merges yet.</li>
        {/each}
      {/if}
    </ul>
  </section>
</div>

<style>
  .head {
    display: flex;
    gap: 20px;
    align-items: flex-end;
    justify-content: space-between;
  }
  .titles {
    display: grid;
    gap: 6px;
    min-width: 0;
  }
  .lead {
    margin: 0;
    max-width: 78ch;
    color: var(--muted);
    font-size: 14.5px;
    /* two lines, so that the tiles do not move when the counts arrive */
    min-height: 3em;
  }
  .big {
    padding: 7px 14px;
    font-size: 14px;
  }

  /* At a glance: four tiles with a coloured top edge, as the deck's cards. */
  .tiles {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(190px, 1fr));
    gap: 12px;
  }
  .tile {
    --tone: var(--ink);
    display: grid;
    gap: 4px;
    align-content: start;
    padding: 12px 16px 14px;
    background: var(--panel);
    border: 1px solid var(--line);
    border-top: 4px solid var(--tone);
    border-radius: var(--radius);
    box-shadow: 0 1px 2px rgb(0 0 0 / 0.05);
    color: var(--ink);
    text-decoration: none;
    min-width: 0;
    transition: border-color var(--t-fast), box-shadow var(--t-fast);
  }
  a.tile:hover {
    box-shadow: var(--shadow);
  }
  .t-info {
    --tone: var(--info);
  }
  .t-ok {
    --tone: var(--ok);
  }
  .t-warn {
    --tone: var(--warn);
  }
  .t-bad {
    --tone: var(--bad);
  }
  .t-idle {
    --tone: var(--line-strong);
  }
  .num {
    font-size: 28px;
    line-height: 1.15;
    font-weight: 700;
    letter-spacing: -0.01em;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .t-bad .num,
  .t-warn .num {
    color: var(--tone);
  }
  .tile .sub {
    font-size: var(--fs-small);
    color: var(--muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-height: 1.5em;
  }

  /* Clusters */
  .sh {
    display: flex;
    align-items: baseline;
    gap: 12px;
    flex-wrap: wrap;
    margin-bottom: 12px;
  }
  .ctiles {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(240px, 100%), 1fr));
    gap: 10px;
  }
  .ctile {
    display: grid;
    gap: 6px;
    padding: 10px 12px;
    border: 1px solid var(--line);
    border-left: 4px solid var(--ok);
    border-radius: var(--radius-s);
    background: var(--panel-2);
    color: var(--ink);
    text-decoration: none;
    min-width: 0;
  }
  .ctile:hover {
    border-color: var(--line-strong);
  }
  .ctile.h-warn {
    border-left-color: var(--warn);
  }
  .ctile.h-bad {
    border-left-color: var(--bad);
  }
  .ctile.h-muted {
    border-left-color: var(--line-strong);
  }
  .ch {
    display: flex;
    align-items: center;
    gap: 7px;
  }
  .ch :global(svg) {
    color: var(--muted);
    flex: none;
  }
  .cname {
    font: 700 15px var(--mono);
  }
  .cmeta {
    color: var(--muted);
    font-size: var(--fs-small);
  }
  .crev {
    display: flex;
    gap: 8px;
    align-items: baseline;
    font-size: 13px;
    min-width: 0;
  }
  .chg {
    color: var(--accent);
  }

  /* Recent merges */
  .merged {
    list-style: none;
    padding: 0;
    margin: 10px 0 0;
    display: grid;
    gap: 8px;
  }
  .merged li {
    display: grid;
    grid-template-columns: 80px minmax(0, 1fr) auto auto 64px;
    gap: 12px;
    align-items: center;
    font-size: 13.5px;
  }
  .merged li.none,
  .merged li.skrow {
    grid-template-columns: minmax(0, 1fr);
  }
  .t {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .skel {
    display: block;
    height: 14px;
    border-radius: 4px;
    background: linear-gradient(90deg, var(--line) 0%, var(--panel-2) 50%, var(--line) 100%);
    background-size: 200% 100%;
    animation: pk-shimmer 1.2s ease-in-out infinite;
  }
  .skel.line {
    display: inline-block;
    width: min(46ch, 100%);
    vertical-align: middle;
  }
  .skrow .skel {
    width: 70%;
    margin: 4px 0;
  }
  @keyframes pk-shimmer {
    to {
      background-position: -200% 0;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .skel {
      animation: none;
    }
  }
</style>
