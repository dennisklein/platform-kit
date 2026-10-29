<script lang="ts">
  // File tree of a worktree (or main) with change flags: + added, M
  // modified, D deleted; directories show how many files changed below.
  import FileTree from './FileTree.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import type { FileNode } from '../../lib/api/types';

  let {
    nodes,
    active,
    open,
    dirty = new Set<string>(),
    depth = 0,
    onopen,
    ontoggle,
  }: {
    nodes: FileNode[];
    active?: string;
    open: Set<string>;
    dirty?: Set<string>;
    depth?: number;
    onopen: (path: string) => void;
    ontoggle: (path: string) => void;
  } = $props();

  const FLAG: Record<string, string> = { added: '+', untracked: '+', modified: 'M', deleted: 'D', renamed: 'R' };

  const sorted = $derived(
    [...nodes].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'dir' ? -1 : 1)),
  );
</script>

<ul class="tree" role={depth === 0 ? 'tree' : 'group'}>
  {#each sorted as n (n.path)}
    {#if n.type === 'dir'}
      {@const isOpen = open.has(n.path)}
      <li role="treeitem" aria-expanded={isOpen} aria-selected="false">
        <button class="node dir" style:--d={depth} onclick={() => ontoggle(n.path)}>
          <span class="chev" class:open={isOpen} aria-hidden="true"><Icon name="chevron-right" size={13} /></span>
          <span class="name">{n.name}</span>
          {#if n.changed}<span class="count" title="{n.changed} changed below">+{n.changed}</span>{/if}
        </button>
        {#if isOpen && n.children?.length}
          <FileTree nodes={n.children} {active} {open} {dirty} depth={depth + 1} {onopen} {ontoggle} />
        {/if}
      </li>
    {:else}
      <li role="treeitem" aria-selected={active === n.path}>
        <button
          class="node file s-{n.status ?? 'none'}"
          class:active={active === n.path}
          style:--d={depth}
          onclick={() => onopen(n.path)}
          title="{n.path}{n.kind ? ` · ${n.kind}` : ''}"
        >
          <svg class="fi" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 1.5h5l3 3v10H4z M9 1.5v3h3" /></svg>
          <span class="name">{n.name}</span>
          {#if dirty.has(n.path)}<span class="dot" title="saved, not committed"></span>{/if}
          {#if n.problems}<span class="prob" title="{n.problems} problems">{n.problems}</span>{/if}
          {#if n.status}<span class="flag">{FLAG[n.status] ?? ''}</span>{/if}
        </button>
      </li>
    {/if}
  {/each}
</ul>

<style>
  .tree {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .node {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 6px;
    border: 0;
    background: none;
    color: var(--ink);
    font: 13.5px var(--mono);
    padding: 4px 12px 4px calc(12px + var(--d) * 16px);
    text-align: left;
    min-width: 0;
  }
  .node:hover {
    background: color-mix(in srgb, var(--panel) 60%, transparent);
  }
  .node.active {
    background: var(--accent-soft);
    font-weight: 700;
  }
  .chev {
    display: inline-block;
    width: 13px;
    height: 13px;
    color: var(--muted);
    transition: transform 0.12s;
    display: inline-flex;
  }
  .chev.open {
    transform: rotate(90deg);
  }
  .fi {
    width: 14px;
    height: 14px;
    fill: none;
    stroke: var(--muted);
    stroke-width: 1.3;
    flex: none;
    margin-left: 2px;
  }
  .name {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .count {
    color: var(--ok);
    font-weight: 700;
    font-size: 12px;
  }
  .flag {
    font-weight: 700;
    font-size: 12px;
    width: 12px;
    text-align: center;
  }
  .s-added .flag,
  .s-untracked .flag {
    color: var(--ok);
  }
  .s-modified .flag,
  .s-renamed .flag {
    color: var(--accent);
  }
  .s-deleted .flag {
    color: var(--bad);
  }
  .s-deleted .name {
    text-decoration: line-through;
    color: var(--muted);
  }
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--accent-fill);
    flex: none;
  }
  .prob {
    font: 600 10.5px var(--sans);
    color: #fff;
    background: var(--bad);
    border-radius: 8px;
    padding: 0 5px;
  }
</style>
