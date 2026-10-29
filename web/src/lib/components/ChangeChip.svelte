<script lang="ts">
  // A linked change: branch icon, id, optional title and its state chip.
  // Direct commits (no change id) link to the history entry.
  import type { ChangeLink } from '../api/ftypes';
  import { href } from '../router.svelte';
  import { isChangeId, short } from '../f/fmt';
  import StateChip from './StateChip.svelte';
  import Glyph from './Glyph.svelte';

  let { change, title = false, to }: { change: ChangeLink; title?: boolean; to?: string } = $props();

  const isChange = $derived(isChangeId(change.id));
  const link = $derived(
    to ?? (isChange ? `/changes/${change.id}` : change.sha ? `/history/${change.sha}` : `/history?q=${change.id}`),
  );
</script>

<a class="cc" href={href(link)} title={change.title}>
  <Glyph name={isChange ? 'branch' : 'commit'} size={14} />
  <span class="mono id">{isChange ? change.id : short(change.sha ?? change.id)}</span>
  {#if title}<span class="t">{change.title}</span>{/if}
  {#if change.state}<span class="st"><StateChip state={change.state} /></span>{/if}
</a>

<style>
  .cc {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px 7px;
    color: var(--ink);
    text-decoration: none;
    min-width: 0;
    font-size: 13px;
  }
  .cc :global(svg) {
    color: var(--accent);
  }
  .id {
    font-size: 12.5px;
    white-space: nowrap;
    flex: none;
  }
  .t {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .st {
    margin-left: auto;
  }
  .cc:hover .id {
    color: var(--accent);
  }
</style>
