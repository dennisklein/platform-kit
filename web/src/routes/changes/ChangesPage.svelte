<script lang="ts">
  // Changes without a selection: start a change, or pick one in the rail.
  // Shows the recent merges of main below.
  import type { Params } from '../../lib/router.svelte';
  import { href, seg } from '../../lib/router.svelte';
  import { api, must } from '../../lib/api/client';
  import type { ChangeSummary } from '../../lib/api/types';
  import { changes } from '../../lib/stores/changes.svelte';
  import { ui } from '../../lib/stores/ui.svelte';
  import { shortSha, timeAgo } from '../../lib/change/format';
  import StateChip from '../../lib/components/StateChip.svelte';
  import Icon from '../../lib/components/Icon.svelte';

  let { query }: { params: Params; query: URLSearchParams } = $props();

  let recent = $state<ChangeSummary[]>([]);
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

  $effect(() => {
    if (query.get('new') === '1') ui.newChangeOpen = true;
  });

  const mine = $derived(changes.list.filter((c) => c.group === 'mine').length);
  const review = $derived(changes.list.filter((c) => c.group === 'review').length);
</script>

<div class="page">
  <section class="hero card">
    <div>
      <h1>Changes</h1>
      <p class="muted">
        {changes.list.length} open: {mine} mine, {review} waiting for my review. Pick one in the rail, or start a change:
        the bench makes the worktree, the branch and the metadata file.
      </p>
    </div>
    <button class="btn dark" onclick={() => (ui.newChangeOpen = true)}><Icon name="plus" size={15} /> New change</button>
  </section>

  <section class="card">
    <div class="eyebrow">Recently merged on main</div>
    <ul>
      {#each recent as c (c.id)}
        <li>
          <a class="mono" href={href(`/changes/${seg(c.id)}`)}>{c.id}</a>
          <span class="t">{c.title}</span>
          <StateChip state={c.state} />
          <span class="muted small">{c.author} · {timeAgo(c.mergedAt)}</span>
          <span class="mono muted small">{shortSha(c.mergeSha)}</span>
        </li>
      {:else}
        <li class="muted small">No merges yet.</li>
      {/each}
    </ul>
  </section>
</div>

<style>
  .hero {
    display: flex;
    gap: 20px;
    align-items: center;
    justify-content: space-between;
    padding: 20px 22px;
  }
  .hero p {
    margin: 6px 0 0;
    max-width: 70ch;
  }
  ul {
    list-style: none;
    padding: 0;
    margin: 10px 0 0;
    display: grid;
    gap: 8px;
  }
  li {
    display: grid;
    grid-template-columns: 80px minmax(0, 1fr) auto auto 64px;
    gap: 12px;
    align-items: center;
    font-size: 13.5px;
  }
  .t {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
