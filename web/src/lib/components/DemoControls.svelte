<script lang="ts">
  // Demo controls (only when the bench runs with PK_DEMO=1): approve a change
  // as anna or jonas in the forge, and make a cluster fail or heal in fluxsim.
  import { api, must, errorText } from '../api/client';
  import type { Cluster } from '../api/ftypes';
  import { session } from '../stores/me.svelte';
  import { changes } from '../stores/changes.svelte';
  import { focus } from '../stores/focus.svelte';
  import { work } from '../stores/context.svelte';
  import { demoTarget } from '../f/demo';
  import Glyph from './Glyph.svelte';

  let {
    changeId: fixedChange,
    sections = ['approve', 'flux'],
    title = 'Demo controls',
    onflux,
  }: {
    changeId?: string;
    sections?: ('approve' | 'flux')[];
    title?: string;
    /** Called with the cluster after a fault was injected or healed. */
    onflux?: (c: Cluster) => void;
  } = $props();

  const demo = $derived(!!(session.status?.demo ?? session.me?.demo));
  const people = ['anna', 'jonas'];

  const candidates = $derived(changes.list.filter((c) => c.mr && c.mr.state === 'open'));
  let picked = $state('');
  // Default: the change on screen or the one the user works on, else one that
  // waits for review. Once approved, the pick stays (the next approval of the
  // same change must not jump to another one when its state moves on).
  const changeId = $derived(
    fixedChange ?? demoTarget(candidates, { picked, onScreen: focus.current?.changeId, working: work.changeId }),
  );

  const target = $derived(changes.byId(changeId || undefined));

  let clusters = $state<Cluster[]>([]);
  let cluster = $state('defiant');
  let busy = $state<string | null>(null);
  let msg = $state<{ ok: boolean; text: string } | null>(null);

  async function loadClusters() {
    try {
      clusters = await must(api.GET('/clusters'));
      if (!clusters.some((c) => c.id === cluster) && clusters[0]) cluster = clusters[0].id;
    } catch {
      clusters = [];
    }
  }

  $effect(() => {
    if (demo && sections.includes('flux')) void loadClusters();
  });

  async function approve(as: string) {
    if (!changeId) return;
    if (!fixedChange) picked = changeId;
    busy = `approve-${as}`;
    msg = null;
    try {
      const a = await must(api.POST('/demo/approve', { body: { changeId, as } }));
      msg = {
        ok: true,
        text: `${as} approved ${changeId}${a.counts ? '' : ` (does not count: ${a.reason ?? 'not an approver'})`}.`,
      };
      void changes.load();
    } catch (e) {
      msg = { ok: false, text: errorText(e) };
    } finally {
      busy = null;
    }
  }

  async function flux(fail: boolean) {
    busy = fail ? 'fail' : 'heal';
    msg = null;
    try {
      const c = await must(api.POST('/demo/flux', { body: { cluster, fail } }));
      clusters = clusters.map((x) => (x.id === c.id ? c : x));
      onflux?.(c);
      msg = { ok: true, text: fail ? `${cluster}: health checks fail now.` : `${cluster}: healed, verifies again shortly.` };
    } catch (e) {
      msg = { ok: false, text: errorText(e) };
    } finally {
      busy = null;
    }
  }

  const failing = $derived(clusters.find((c) => c.id === cluster)?.failing ?? false);
</script>

{#if demo}
  <section class="demo" aria-label={title}>
    <header>
      <Glyph name="zap" size={14} />
      <h3>{title}</h3>
      <span class="badge">PK_DEMO</span>
    </header>

    {#if sections.includes('approve')}
      <div class="grp">
        <span class="lbl">Approve in the forge</span>
        <div class="row">
          {#if !fixedChange}
            <select class="select" bind:value={picked} aria-label="Change to approve">
              <option value="">{changeId || 'no open MR'}</option>
              {#each candidates as c (c.id)}
                <option value={c.id}>{c.id} · {c.title}</option>
              {/each}
            </select>
          {/if}
          {#each people as p (p)}
            <button class="btn small" disabled={!changeId || !!busy} onclick={() => approve(p)}>
              <Glyph name="check" size={13} />{busy === `approve-${p}` ? 'Approving…' : `Approve as ${p}`}
            </button>
          {/each}
        </div>
        {#if target}
          <span class="small muted">
            <span class="mono">{target.id}</span> · {target.state} · {target.approvals.have} of {target.approvals.need} approvals
          </span>
        {/if}
      </div>
    {/if}

    {#if sections.includes('flux')}
      <div class="grp">
        <span class="lbl">Flux (fluxsim)</span>
        <div class="row">
          <select class="select" bind:value={cluster} aria-label="Cluster">
            {#each clusters.length ? clusters : [{ id: 'defiant' }] as c (c.id)}
              <option value={c.id}>{c.id}</option>
            {/each}
          </select>
          {#if failing}
            <button class="btn small" disabled={!!busy} onclick={() => flux(false)}>
              <Glyph name="refresh" size={13} />{busy === 'heal' ? 'Healing…' : 'Heal'}
            </button>
          {:else}
            <button class="btn small danger" disabled={!!busy} onclick={() => flux(true)}>
              <Glyph name="alert" size={13} />{busy === 'fail' ? 'Failing…' : 'Make it fail'}
            </button>
          {/if}
        </div>
      </div>
    {/if}

    {#if msg}<p class="small {msg.ok ? 'ok-text' : 'bad-text'}">{msg.text}</p>{/if}
  </section>
{/if}

<style>
  .demo {
    border: 1px dashed color-mix(in srgb, var(--accent-fill) 70%, transparent);
    background: color-mix(in srgb, var(--accent-soft) 45%, var(--panel));
    border-radius: var(--radius);
    padding: 10px 14px;
    display: grid;
    gap: 8px;
  }
  header {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--accent);
  }
  header h3 {
    color: var(--accent);
  }
  .grp {
    display: grid;
    gap: 4px;
  }
  .lbl {
    font-size: 12px;
    color: var(--muted);
  }
  .select {
    max-width: 280px;
  }
  p {
    margin: 0;
  }
</style>
