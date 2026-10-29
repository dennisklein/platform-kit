<script lang="ts">
  // Clusters: what Flux applied and verified on each cluster, and the ring
  // rollout (canary, then prod) with promote / hold / release.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { Cluster, RingStatus, Revision } from '../../lib/api/ftypes';
  import { events } from '../../lib/stores/events.svelte';
  import { session } from '../../lib/stores/me.svelte';
  import { toasts } from '../../lib/stores/toasts.svelte';
  import { short, ago, endsAt, isChangeId } from '../../lib/f/fmt';
  import DemoControls from '../../lib/components/DemoControls.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  let clusters = $state<Cluster[] | null>(null);
  let rings = $state<RingStatus[]>([]);
  let error = $state<string | null>(null);
  let busy = $state<string | null>(null);
  let loading = $state(false);

  async function load() {
    loading = true;
    try {
      const [c, r] = await Promise.all([must(api.GET('/clusters')), must(api.GET('/rings')).catch(() => [] as RingStatus[])]);
      clusters = c;
      rings = r;
      error = null;
    } catch (e) {
      error = errorText(e);
    } finally {
      loading = false;
    }
  }
  $effect(() => {
    void load();
  });

  // Flux and merges change what the clusters run: reload on those events.
  $effect(() => {
    let t: ReturnType<typeof setTimeout> | undefined;
    const later = () => {
      clearTimeout(t);
      t = setTimeout(() => void load(), 300);
    };
    const offs = ['flux.updated', 'forge.updated', 'change.updated'].map((e) => events.on(e as any, later));
    return () => {
      clearTimeout(t);
      offs.forEach((o) => o());
    };
  });

  const ringOrder = $derived(rings.map((r) => r.ring));
  const sorted = $derived(
    [...(clusters ?? [])].sort((a, b) => {
      const ra = ringOrder.indexOf(a.ring);
      const rb = ringOrder.indexOf(b.ring);
      return (ra < 0 ? 99 : ra) - (rb < 0 ? 99 : rb) || a.id.localeCompare(b.id);
    }),
  );
  const highlight = $derived(params.id ?? query.get('cluster') ?? undefined);

  type Health = { tone: 'ok' | 'warn' | 'bad' | 'muted'; label: string };
  function health(c: Cluster): Health {
    if (c.reachable === false) return { tone: 'muted', label: 'unreachable' };
    if (c.failing || !c.healthy) return { tone: 'bad', label: 'failing' };
    if (c.held) return { tone: 'warn', label: 'held' };
    if (c.pending && c.pending.sha !== c.applied?.sha) return { tone: 'warn', label: 'reconciling' };
    if (c.applied && c.verified?.sha !== c.applied.sha) return { tone: 'warn', label: 'verifying' };
    return { tone: 'ok', label: 'healthy' };
  }

  const ringFailing = (r: RingStatus) =>
    (clusters ?? []).some((c) => (r.clusters ?? []).includes(c.id) && c.reachable !== false && (c.failing || !c.healthy));
  const ringHeld = (ring: string) => rings.find((r) => r.ring === ring)?.held ?? false;

  async function act(r: RingStatus, what: 'promote' | 'hold' | 'release') {
    busy = `${what}-${r.ring}`;
    try {
      let next: RingStatus;
      if (what === 'promote') next = await must(api.POST('/rings/{ring}/promote', { params: { path: { ring: r.ring } }, body: {} }));
      else if (what === 'hold') next = await must(api.POST('/rings/{ring}/hold', { params: { path: { ring: r.ring } } }));
      else next = await must(api.POST('/rings/{ring}/release', { params: { path: { ring: r.ring } } }));
      rings = rings.map((x) => (x.ring === r.ring ? next : x));
      const verb = { promote: 'promoted', hold: 'held', release: 'released' }[what];
      toasts.ok(
        `Ring ${r.ring} ${verb}.`,
        what === 'promote' ? `${next.tag ?? `ring/${r.ring}`} → ${short(next.sha)}${next.change ? ` (${next.change})` : ''}` : undefined,
      );
      void load();
    } catch (e) {
      toasts.error(`Could not ${what} ${r.ring}`, e);
    } finally {
      busy = null;
    }
  }

  // A held ring can be promoted (to roll a revert forward); the hold ends
  // when that revision verifies. The bench decides with the gate.
  const canPromote = (r: RingStatus) => !!r.candidate && r.candidate !== r.sha && r.gate !== 'none';
  const promoteTitle = (r: RingStatus) =>
    !canPromote(r)
      ? r.reason || 'Nothing to promote'
      : `Move ${r.tag ?? `ring/${r.ring}`} to ${short(r.candidate)}${r.held ? '; the hold ends when it verifies' : ''}`;
  const mainSha = $derived(session.status?.main);
</script>

{#snippet rev(label: string, r: Revision | undefined, hint?: string)}
  <div class="rev">
    <span class="rl">{label}</span>
    {#if r}
      <a class="mono sha" href={href(`/history/${r.sha}`)} title={r.sha}>{short(r.sha)}</a>
      {#if r.change}
        <a class="mono chg" href={href(isChangeId(r.change) ? `/changes/${r.change}` : `/history`, isChangeId(r.change) ? undefined : { q: r.change })}>{r.change}</a>
      {/if}
      {#if r.at}<span class="muted small">{ago(r.at)}</span>{/if}
    {:else}
      <span class="muted small">{hint ?? '—'}</span>
    {/if}
  </div>
{/snippet}

<div class="page">
  <header class="head">
    <div>
      <h1>Clusters</h1>
      <p class="muted small sub">
        Flux applies main{rings.some((r) => r.following && r.following !== 'main') ? ' through ring tags' : ''} to each cluster.
        A change is <strong>deployed</strong> when Flux applied it, <strong>verified</strong> when health checks pass.
      </p>
    </div>
    <span class="spacer"></span>
    <button class="btn small" onclick={() => load()} disabled={loading} title="Reload">
      {#if loading}<span class="spin"></span>{:else}<Glyph name="refresh" size={13} />{/if} Reload
    </button>
  </header>

  {#if error}
    <div class="error-box">{error}</div>
  {/if}

  {#if rings.length}
    <section aria-label="Rings">
      <h2 class="eyebrow">Rings</h2>
      <div class="flow">
        <div class="node main">
          <div class="nh"><Glyph name="branch" size={14} /> <strong>main</strong></div>
          {#if mainSha}
            <div class="rev">
              <a class="mono sha" href={href(`/history/${mainSha.sha}`)}>{short(mainSha.sha)}</a>
              {#if mainSha.change}<a class="mono chg" href={href(`/changes/${mainSha.change}`)}>{mainSha.change}</a>{/if}
            </div>
            {#if mainSha.subject}<p class="small muted subj">{mainSha.subject}</p>{/if}
          {/if}
        </div>
        {#each rings as r (r.ring)}
          <div class="arrow" aria-hidden="true"><Glyph name="arrow-right" size={16} /></div>
          <div class="node ring" class:held={r.held}>
            <div class="nh">
              <Glyph name="tag" size={14} />
              <strong>{r.ring}</strong>
              <span class="mono small muted">{r.tag ?? (r.following === 'main' ? 'follows main' : r.following)}</span>
              <span class="spacer"></span>
              {#if r.held}
                <span class="pill warn"><span class="dot"></span>held{r.heldBy ? ` by ${r.heldBy}` : ''}</span>
              {:else if ringFailing(r)}
                <span class="pill bad"><span class="dot"></span>failing</span>
              {:else if r.verified}
                <span class="pill ok"><span class="dot"></span>verified</span>
              {:else}
                <span class="pill"><span class="dot"></span>rolling out</span>
              {/if}
            </div>
            <div class="rows">
              {@render rev('runs', r.sha ? { sha: r.sha, change: r.change } : undefined, r.following === 'main' ? 'main head' : 'no tag yet')}
              {#if r.candidate && r.candidate !== r.sha}
                {@render rev('next', { sha: r.candidate, change: r.candidateChange })}
              {/if}
            </div>
            <div class="gate">
              {#if r.gate}
                <span class="badge g-{r.gate}">{r.gate === 'auto' ? 'auto after soak' : r.gate === 'manual' ? 'manual promote' : 'nothing to promote'}</span>
              {/if}
              {#if r.soakUntil && r.gate === 'auto'}
                <span class="small muted"><Glyph name="clock" size={12} /> soak {endsAt(r.soakUntil)}</span>
              {/if}
            </div>
            {#if r.reason}<p class="reason small">{r.reason}</p>{/if}
            <div class="cl">
              {#each r.clusters ?? [] as c (c)}<a class="cchip mono" href={href('/clusters', { cluster: c })}>{c}</a>{/each}
            </div>
            <div class="acts">
              <button class="btn small primary" disabled={!canPromote(r) || !!busy} onclick={() => act(r, 'promote')} title={promoteTitle(r)}>
                {#if busy === `promote-${r.ring}`}<span class="spin"></span>{:else}<Glyph name="play" size={12} />{/if} Promote
              </button>
              {#if r.held}
                <button class="btn small" disabled={!!busy} onclick={() => act(r, 'release')}>
                  {#if busy === `release-${r.ring}`}<span class="spin"></span>{:else}<Glyph name="play" size={12} />{/if} Release
                </button>
              {:else}
                <button class="btn small" disabled={!!busy} onclick={() => act(r, 'hold')} title="Stop promotions to this ring">
                  {#if busy === `hold-${r.ring}`}<span class="spin"></span>{:else}<Glyph name="pause" size={12} />{/if} Hold
                </button>
              {/if}
            </div>
          </div>
        {/each}
      </div>
    </section>
  {/if}

  <section aria-label="Clusters">
    <h2 class="eyebrow">Clusters</h2>
    {#if clusters === null && !error}
      <p class="muted small">Loading…</p>
    {:else if clusters && !clusters.length}
      <div class="empty">No clusters in the state repo.</div>
    {:else}
      <div class="grid">
        {#each sorted as c (c.id)}
          {@const h = health(c)}
          <article class="cluster t-{h.tone}" class:hl={highlight === c.id} id="cluster-{c.id}">
            <header class="ch">
              <Glyph name="hex" size={18} />
              <h3 class="mono">{c.id}</h3>
              <span class="pill {h.tone === 'muted' ? '' : h.tone}"><span class="dot"></span>{h.label}</span>
              <span class="spacer"></span>
              <span class="badge">{c.role}</span>
              <span class="badge ring-b" title={c.ringTag ? `follows ${c.ringTag}` : undefined}>ring {c.ring}{ringHeld(c.ring) ? ' · held' : ''}</span>
            </header>
            <div class="rows">
              {@render rev('applied', c.applied, c.reachable === false ? 'unknown' : 'nothing yet')}
              {@render rev('verified', c.verified, c.reachable === false ? 'unknown' : 'not yet')}
              {#if c.pending && c.pending.sha !== c.applied?.sha}
                {@render rev('pending', c.pending)}
              {/if}
            </div>
            {#if c.message}
              <p class="msg small" class:bad={h.tone === 'bad'}>
                {#if h.tone === 'bad'}<Glyph name="alert" size={13} />{/if}{c.message}
              </p>
            {/if}
            {#if c.kustomizations?.length}
              <table class="table ks">
                <thead><tr><th>Kustomization</th><th>Path</th><th>Revision</th></tr></thead>
                <tbody>
                  {#each c.kustomizations as k (k.name)}
                    <tr title={k.message}>
                      <td><span class="kd" class:ok={k.ready} aria-label={k.ready ? 'ready' : 'not ready'}></span><span class="mono">{k.name}</span></td>
                      <td class="mono muted">{k.path ?? ''}</td>
                      <td class="mono">{k.revision ? short(k.revision.split(':').pop()) : '—'}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            {/if}
            <footer class="cf small muted">
              {#if c.path}<a class="mono" href={href('/files', { path: c.path })}>{c.path}</a>{/if}
              <span class="spacer"></span>
              {#if c.objects != null}<span>{c.objects} objects</span>{/if}
              <a href={href('/history', { path: `clusters/${c.id}/**` })}>history</a>
            </footer>
          </article>
        {/each}
      </div>
    {/if}
  </section>

  <DemoControls
    sections={['flux']}
    title="Demo: Flux"
    onflux={(c) => {
      if (clusters) clusters = clusters.map((x) => (x.id === c.id ? c : x));
      setTimeout(() => void load(), 600);
    }}
  />
</div>

<style>
  /* One column that may shrink below its content (phone width). */
  .page {
    grid-template-columns: minmax(0, 1fr);
  }
  .head {
    display: flex;
    align-items: flex-start;
    gap: 12px;
  }
  h1 {
    margin: 0;
  }
  .sub {
    margin: 4px 0 0;
    max-width: 680px;
  }
  h2.eyebrow {
    margin: 0 0 8px;
  }
  .flow {
    display: flex;
    align-items: stretch;
    gap: 8px;
    overflow-x: auto;
    padding-bottom: 4px;
  }
  .arrow {
    display: grid;
    place-items: center;
    color: var(--muted);
    flex: none;
  }
  .node {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    background: var(--panel);
    padding: 10px 12px;
    display: grid;
    gap: 8px;
    align-content: start;
    min-width: 0;
  }
  .node.main {
    flex: 0 0 200px;
    background: var(--bg, var(--panel));
  }
  .node.ring {
    flex: 1 1 320px;
    max-width: 460px;
  }
  .node.held {
    border-color: color-mix(in srgb, var(--warn) 50%, transparent);
    background: var(--warn-soft);
  }
  .nh {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .subj {
    margin: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .rows {
    display: grid;
    gap: 3px;
  }
  .rev {
    display: flex;
    align-items: baseline;
    gap: 8px;
    font-size: 13px;
    min-width: 0;
  }
  .rl {
    width: 64px;
    flex: none;
    color: var(--muted);
    font-size: 12px;
  }
  .sha {
    color: var(--ink);
  }
  .chg {
    color: var(--accent);
  }
  .gate {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .gate .small {
    display: inline-flex;
    gap: 4px;
    align-items: center;
  }
  .g-manual {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .g-auto {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .reason {
    margin: 0;
    color: var(--muted);
  }
  .cl {
    display: flex;
    gap: 4px;
    flex-wrap: wrap;
  }
  .cchip {
    font-size: 12px;
    padding: 1px 7px;
    border-radius: 4px;
    background: var(--accent-soft);
    color: var(--accent);
  }
  .acts {
    display: flex;
    gap: 6px;
  }
  .acts .btn {
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(360px, 100%), 1fr));
    gap: 12px;
  }
  .cluster {
    border: 1px solid var(--line);
    border-left: 4px solid var(--ok);
    border-radius: var(--radius);
    padding: 12px 14px;
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 10px;
    align-content: start;
    background: var(--panel);
    min-width: 0;
  }
  .cluster.t-bad {
    border-left-color: var(--bad);
  }
  .cluster.t-warn {
    border-left-color: var(--warn);
  }
  .cluster.t-muted {
    border-left-color: var(--line-strong);
  }
  .cluster.hl {
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-fill) 60%, transparent);
  }
  .ch {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .ch h3 {
    margin: 0;
    font-size: 16px;
    text-transform: none;
    letter-spacing: 0;
    color: var(--ink);
  }
  .ring-b {
    background: var(--accent-soft);
    color: var(--accent);
  }
  .msg {
    margin: 0;
    color: var(--muted);
    display: flex;
    gap: 6px;
    align-items: flex-start;
    overflow-wrap: anywhere;
  }
  .msg.bad {
    color: var(--bad);
  }
  .ks {
    font-size: 12.5px;
  }
  .ks td {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 180px;
  }
  .kd {
    display: inline-block;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--bad);
    margin-right: 6px;
  }
  .kd.ok {
    background: var(--ok);
  }
  .cf {
    display: flex;
    gap: 10px;
    align-items: center;
  }
  .cf a {
    color: var(--muted);
  }
  @media (max-width: 720px) {
    .flow {
      flex-direction: column;
    }
    .arrow {
      transform: rotate(90deg);
    }
    .node.ring,
    .node.main {
      flex: none;
      max-width: none;
    }
    .grid {
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
