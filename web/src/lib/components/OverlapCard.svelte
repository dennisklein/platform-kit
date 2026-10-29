<script lang="ts">
  // Other open changes touch the same rendered objects. No lock, no block.
  import type { Overlap } from '../api/types';
  import { href, seg } from '../router.svelte';
  import Icon from './Icon.svelte';

  let { overlaps, compact = false }: { overlaps: Overlap[]; compact?: boolean } = $props();
  const objName = (o: Overlap['objects'][number]) => `${o.kind}/${o.name}`;
</script>

{#each overlaps as o (o.change)}
  <div class="callout warn overlap">
    <Icon name="warning" size={18} />
    <div>
      {#if compact}
        Overlap with <a href={href(`/changes/${seg(o.change)}`)}><b>{o.change}</b></a> ({o.by}): also changes
        {#each o.objects as ob, i (i)}{i ? ', ' : ''}<code>{ob.name}</code>{/each}.
      {:else}
        <a href={href(`/changes/${seg(o.change)}`)}><b>{o.change}</b></a> ({o.by}) also changes
        {#each o.objects as ob, i (i)}{i ? ', ' : ''}<code class="obj">{objName(ob)}</code>{/each}
        on {[...new Set(o.objects.map((x) => x.cluster))].join(', ')}.
        <div class="muted small">{o.title ?? ''} · No lock, no block: the second merge renders again.</div>
      {/if}
    </div>
  </div>
{/each}

<style>
  .overlap {
    font-size: 14px;
    line-height: 1.55;
  }
  .overlap a {
    color: var(--ink);
  }
  .obj {
    font-size: 13.5px;
    color: var(--ink);
  }
</style>
