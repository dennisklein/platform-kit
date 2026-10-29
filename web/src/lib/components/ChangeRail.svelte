<script lang="ts">
  // Left rail of open changes, grouped as in the change-rail mock: mine,
  // pushed by others, waiting for my review, bots. Live via the changes
  // store (SSE). Filters, search and sort are kept per browser.
  import { changes } from '../stores/changes.svelte';
  import { router, href, seg } from '../router.svelte';
  import { railGroups, railHints, groupCounts, type RailFilter, type RailSort } from '../change/rail';
  import { presenceFor } from '../change/presence.svelte';
  import { shortSha, timeAgo } from '../change/format';
  import { flip } from 'svelte/animate';
  import { fade } from 'svelte/transition';
  import { ms } from '../f/motion';
  import StateChip from './StateChip.svelte';
  import Icon from './Icon.svelte';

  let { onNew }: { onNew?: () => void } = $props();

  function load<T extends string>(key: string, fallback: T, allowed: readonly T[]): T {
    try {
      const v = localStorage.getItem(key) as T | null;
      return v && allowed.includes(v) ? v : fallback;
    } catch {
      return fallback;
    }
  }
  function save(key: string, v: string) {
    try {
      localStorage.setItem(key, v);
    } catch {
      // private mode
    }
  }

  let filter = $state<RailFilter>(load('pk.rail.filter', 'all', ['all', 'mine', 'others', 'review', 'bots'] as const));
  let sort = $state<RailSort>(load('pk.rail.sort', 'updated', ['updated', 'state', 'title'] as const));
  let query = $state('');

  const FILTERS: { key: RailFilter; label: string }[] = [
    { key: 'all', label: 'All open' },
    { key: 'mine', label: 'Mine' },
    { key: 'review', label: 'My reviews' },
    { key: 'bots', label: 'Bots' },
  ];

  const groups = $derived(railGroups(changes.list, filter, sort, query, changes.recent));
  const counts = $derived(groupCounts(changes.list));
  const selected = $derived(router.params.id);

  function setFilter(f: RailFilter) {
    filter = f;
    save('pk.rail.filter', f);
  }
  function setSort(s: RailSort) {
    sort = s;
    save('pk.rail.sort', s);
  }
</script>

<nav class="rail" aria-label="Open changes">
  <div class="tools">
    <div class="filters" role="group" aria-label="Filter">
      {#each FILTERS as f (f.key)}
        <button
          class="filter"
          aria-pressed={filter === f.key}
          onclick={() => setFilter(f.key)}
          title="{counts[f.key]} {f.key === 'all' ? 'open changes' : 'changes'}"
        >
          {f.label}
        </button>
      {/each}
    </div>
  </div>
  <div class="tools2">
    <label class="search">
      <Icon name="search" size={14} />
      <input type="search" placeholder="Filter by id, title, author" bind:value={query} aria-label="Filter changes" />
    </label>
    <select class="sort" value={sort} onchange={(e) => setSort(e.currentTarget.value as RailSort)} aria-label="Sort">
      <option value="updated">Recent</option>
      <option value="state">State</option>
      <option value="title">Title</option>
    </select>
    {#if onNew}
      <button class="btn small primary new" onclick={onNew} title="New change (worktree, branch, metadata)">
        <Icon name="plus" size={14} /> New
      </button>
    {/if}
  </div>

  <div class="list">
    {#if changes.error}
      <div class="error-box">{changes.error}</div>
    {/if}

    {#each groups as g (g.key)}
      <div class="group">{g.label}</div>
      {#each g.items as { c, depth } (c.id)}
        {@const p = presenceFor(c.id, c.presence)}
        <a
          animate:flip={{ duration: ms(220) }}
          in:fade={{ duration: ms(180) }}
          class="item"
          class:child={depth > 0}
          style:--depth={depth}
          href={href(`/changes/${seg(c.id)}`)}
          aria-current={selected === c.id ? 'true' : undefined}
          data-change={c.id}
        >
          <span class="title">{c.title}</span>
          <span class="chips">
            {#if p}<span class="agent" title="{p.actor}: {p.text ?? p.tool ?? ''}"><Icon name="bot" size={13} /></span>{/if}
            <StateChip state={c.state} labState={c.labState} short />
          </span>
          <span class="sub">
            <span class="mono id">{c.id}</span>
            {#each railHints(c) as h, i (i)}
              <span class="hint {h.tone ?? ''}" title={h.title}>{h.text}</span>
            {/each}
          </span>
        </a>
      {/each}
    {:else}
      {#if !changes.loading && !changes.error}
        <p class="empty">
          {#if query}No change matches “{query}”.{:else if changes.list.length}Nothing in this filter.{:else}No open changes.{/if}
        </p>
      {/if}
    {/each}
  </div>

  {#if changes.main}
    <a class="main-ref small" href={changes.main.change ? href(`/history`, { q: changes.main.change }) : href('/history')} title={changes.main.subject}>
      <Icon name="branch" size={13} />
      <span>main</span>
      <span class="mono">{shortSha(changes.main.sha)}</span>
      {#if changes.main.change}<span class="mono">{changes.main.change}</span>{/if}
      <span class="spacer"></span>
      <span>{timeAgo(changes.main.at)}</span>
    </a>
  {/if}
</nav>

<style>
  .rail {
    display: flex;
    flex-direction: column;
    min-height: 0;
    height: 100%;
  }
  .tools {
    display: flex;
    gap: 6px;
    align-items: center;
    padding: 12px 12px 6px;
  }
  .filters {
    display: flex;
    gap: 5px;
    flex: 1;
  }
  .filter {
    font-size: 12.5px;
    padding: 3px 9px;
    white-space: nowrap;
    border-radius: 5px;
    border: 1px solid var(--line-strong);
    background: var(--panel);
    color: var(--ink);
  }
  .filter[aria-pressed='true'] {
    background: var(--gsi-navy);
    border-color: var(--gsi-navy);
    color: #fff;
  }
  :global(:root[data-theme='dark']) .filter[aria-pressed='true'] {
    background: var(--accent-fill);
    border-color: var(--accent-fill);
    color: var(--accent-on);
  }
  .new {
    flex: none;
  }
  .tools2 {
    display: flex;
    gap: 6px;
    padding: 0 12px 10px;
    border-bottom: 1px solid var(--line);
  }
  .search {
    flex: 1;
    display: flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--line-strong);
    background: var(--panel);
    border-radius: 6px;
    padding: 0 8px;
    color: var(--muted);
    min-width: 0;
  }
  .search input {
    border: 0;
    outline: 0;
    background: transparent;
    padding: 4px 0;
    font-size: 12.5px;
    min-width: 0;
    flex: 1;
  }
  .sort {
    border: 1px solid var(--line-strong);
    background: var(--panel);
    border-radius: 6px;
    font-size: 12.5px;
    padding: 2px 4px;
  }
  .list {
    position: relative;
    flex: 1;
    overflow-y: auto;
    padding: 4px 8px 12px;
    display: flex;
    flex-direction: column;
    gap: 1px;
  }
  .group {
    font: 600 11px/1 var(--mono);
    text-transform: uppercase;
    letter-spacing: 0.14em;
    color: var(--muted);
    padding: 14px 10px 6px;
  }
  .item {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 3px 8px;
    text-decoration: none;
    color: inherit;
    border-radius: 6px;
    padding: 7px 10px 8px;
    border-left: 3px solid transparent;
    position: relative;
  }
  .item:hover {
    background: color-mix(in srgb, var(--panel) 70%, transparent);
  }
  .item[aria-current='true'] {
    background: var(--panel);
    border-left-color: var(--accent-fill);
    box-shadow: var(--shadow);
  }
  .item.child {
    margin-left: calc(var(--depth) * 18px);
  }
  .item.child::before {
    content: '';
    position: absolute;
    left: -12px;
    top: -4px;
    height: 20px;
    width: 9px;
    border-left: 1.5px solid var(--line-strong);
    border-bottom: 1.5px solid var(--line-strong);
    border-bottom-left-radius: 5px;
  }
  .title {
    font-weight: 600;
    font-size: 14px;
    min-width: 0;
    overflow-wrap: anywhere;
    line-height: 1.35;
  }
  .chips {
    display: inline-flex;
    gap: 4px;
    align-items: flex-start;
  }
  .agent {
    display: inline-grid;
    place-items: center;
    width: 20px;
    height: 20px;
    border-radius: 50%;
    background: var(--info);
    color: var(--panel);
  }
  .sub {
    grid-column: 1 / -1;
    display: flex;
    flex-wrap: wrap;
    gap: 2px 10px;
    color: var(--muted);
    font-size: 12.5px;
  }
  .id {
    font-size: 12px;
  }
  .hint.warn {
    color: var(--warn);
    font-weight: 600;
  }
  .hint.info {
    color: var(--info);
  }
  .hint.bad {
    color: var(--bad);
  }
  .main-ref {
    display: flex;
    gap: 6px;
    align-items: center;
    padding: 9px 14px;
    border-top: 1px solid var(--line);
    color: var(--muted);
    text-decoration: none;
  }
  .main-ref:hover {
    color: var(--ink);
  }
</style>
