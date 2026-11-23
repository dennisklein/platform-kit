<script lang="ts">
  // Rollout of a merged change: per cluster, the steps from the merge
  // commit to "verified", live from the Flux status of the clusters.
  import { onMount } from 'svelte';
  import type { Change, Cluster } from '../../lib/api/types';
  import { api, must } from '../../lib/api/client';
  import { href } from '../../lib/router.svelte';
  import { shortSha, timeAgo } from '../../lib/change/format';
  import Icon from '../../lib/components/Icon.svelte';
  import { rolloutSteps } from './rollout';

  let { c }: { c: Change } = $props();

  let clusters = $state<Cluster[]>([]);
  const rollout = $derived(c.rollout ?? []);
  const allVerified = $derived(rollout.length > 0 && rollout.every((r) => r.verified));

  async function load() {
    try {
      clusters = await must(api.GET('/clusters'));
    } catch {
      // Flux unreachable: the steps follow the change state alone.
    }
  }

  onMount(() => {
    void load();
    // While Flux works, poll its status (about 10 s from merge to verified).
    const t = setInterval(() => {
      if (!allVerified) void load();
    }, 1500);
    return () => clearInterval(t);
  });

  const byId = $derived(new Map(clusters.map((x) => [x.id, x])));
</script>

<section class="card rollout">
  <div class="top">
    <div class="eyebrow">Rollout</div>
    {#if allVerified}
      <span class="badge ok"><Icon name="check" size={13} />verified</span>
    {:else if rollout.length}
      <span class="badge run"><span class="spin"></span>Flux is applying</span>
    {/if}
  </div>
  <p class="small merged-line">
    Merged into {c.target}{#if c.mergeSha}{' '}as <a class="mono" href={href(`/history/${c.mergeSha}`)}>{shortSha(c.mergeSha)}</a>{/if}{#if c.mergedAt}, {timeAgo(c.mergedAt)}{/if}.
  </p>
  {#each rollout as r (r.cluster)}
    {@const cl = byId.get(r.cluster)}
    {@const steps = rolloutSteps(r, cl, c.mergeSha ?? r.sha ?? '')}
    <div class="cluster">
      <div class="chead">
        <Icon name="clusters" size={16} />
        <b>{r.cluster}</b>
        {#if r.ring}<span class="muted small">{r.ring}</span>{/if}
        <span class="spacer"></span>
        <span class="res r-{r.verified ? 'pass' : r.deployed ? 'running' : 'none'}">{r.verified ? 'verified' : r.deployed ? 'deployed' : 'pending'}</span>
        {#if r.sha}<span class="mono muted small">{shortSha(r.sha)}</span>{/if}
        {#if r.at}<span class="muted small">{timeAgo(r.at)}</span>{/if}
      </div>
      <ol class="steps">
        {#each steps as s (s.key)}
          <li class="s-{s.status}">
            <span class="mk">
              {#if s.status === 'done'}<Icon name="check" size={14} />
              {:else if s.status === 'fail'}<Icon name="x" size={14} />
              {:else if s.status === 'now'}<span class="spin"></span>
              {:else}<span class="hollow"></span>{/if}
            </span>
            <span class="st">{s.title}</span>
            {#if s.detail}<span class="muted small">{s.detail}</span>{/if}
          </li>
        {/each}
      </ol>
      {#if cl?.kustomizations?.length && !r.verified}
        <div class="kusts mono small">
          {#each cl.kustomizations as k (k.name)}
            <span class:ready={k.ready && !!c.mergeSha && (k.revision ?? '').includes(c.mergeSha)} title={k.message}>{k.name}</span>
          {/each}
        </div>
      {/if}
    </div>
  {:else}
    <p class="muted small">No cluster reports yet. Flux applies the merge commit next.</p>
  {/each}
</section>

<style>
  .rollout {
    display: grid;
    gap: 10px;
    align-content: start;
  }
  .top {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    font-weight: 700;
    padding: 1px 9px;
    border-radius: 999px;
  }
  .badge.ok {
    background: var(--ok-soft);
    color: var(--ok);
    animation: pop 0.5s ease-out;
  }
  .badge.run {
    background: var(--warn-soft);
    color: var(--warn);
  }
  @keyframes pop {
    0% {
      transform: scale(0.8);
      opacity: 0;
    }
    60% {
      transform: scale(1.08);
    }
    100% {
      transform: scale(1);
      opacity: 1;
    }
  }
  .merged-line {
    margin: 0;
  }
  .cluster {
    border: 1px solid var(--line);
    border-radius: var(--radius-s);
    padding: 10px 14px 12px;
    display: grid;
    gap: 8px;
  }
  .chead {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    font-size: 14.5px;
  }
  .chead > :global(svg) {
    color: var(--muted);
  }
  .steps {
    list-style: none;
    margin: 0;
    padding: 0 0 0 4px;
    display: grid;
    gap: 2px;
    position: relative;
  }
  .steps li {
    display: grid;
    grid-template-columns: 20px auto 1fr;
    gap: 10px;
    align-items: center;
    font-size: 14.5px;
    padding: 3px 0;
    color: var(--muted);
    transition: color 0.3s ease;
  }
  .steps li.s-done {
    color: var(--ink);
  }
  .steps li.s-now {
    color: var(--ink);
    font-weight: 600;
  }
  .steps li.s-fail {
    color: var(--bad);
  }
  .mk {
    display: inline-flex;
    justify-content: center;
    color: var(--muted);
  }
  .s-done .mk {
    color: var(--ok);
  }
  .s-now .mk {
    color: var(--warn);
  }
  .s-fail .mk {
    color: var(--bad);
  }
  .hollow {
    width: 12px;
    height: 12px;
    border-radius: 50%;
    border: 2px solid var(--line-strong);
    box-sizing: border-box;
  }
  .spin {
    width: 12px;
    height: 12px;
    border: 2px solid currentColor;
    border-right-color: transparent;
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }
  @keyframes spin {
    to {
      transform: rotate(360deg);
    }
  }
  .kusts {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    padding-left: 34px;
  }
  .kusts span {
    padding: 0 7px;
    border-radius: 4px;
    background: var(--panel-2);
    color: var(--muted);
    transition:
      background-color 0.4s ease,
      color 0.4s ease;
  }
  .kusts span.ready {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .res {
    font-size: 11.5px;
    font-weight: 600;
    padding: 0 7px;
    border-radius: 4px;
    background: var(--line);
    color: var(--muted);
  }
  .res.r-pass {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .res.r-running {
    background: var(--warn-soft);
    color: var(--warn);
  }
  @media (prefers-reduced-motion: reduce) {
    .badge.ok {
      animation: none;
    }
  }
</style>
