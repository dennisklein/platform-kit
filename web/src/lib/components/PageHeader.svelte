<script lang="ts">
  // The header of a top-level page: an optional eyebrow (a back link), the
  // h1, one line or two of meta text and the page actions on the right. All
  // pages use it, so their titles start at the same x and y.
  import type { Snippet } from 'svelte';

  let {
    title,
    eyebrow,
    meta,
    actions,
  }: { title: string; eyebrow?: Snippet; meta?: Snippet; actions?: Snippet } = $props();
</script>

<header class="ph">
  <div class="tx">
    {#if eyebrow}<div class="eb small">{@render eyebrow()}</div>{/if}
    <h1>{title}</h1>
    {#if meta}<div class="meta">{@render meta()}</div>{/if}
  </div>
  {#if actions}<div class="acts">{@render actions()}</div>{/if}
</header>

<style>
  .ph {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 12px 20px;
    flex-wrap: wrap;
    min-width: 0;
  }
  .tx {
    display: grid;
    gap: 6px;
    min-width: 0;
    flex: 1 1 420px;
  }
  .eb {
    margin-bottom: -2px;
  }
  .eb :global(a) {
    color: var(--muted);
  }
  .meta {
    margin: 0;
    max-width: 78ch;
    color: var(--muted);
    font-size: 14px;
    line-height: 1.55;
  }
  .meta :global(p) {
    margin: 0;
  }
  .acts {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    flex: none;
    max-width: 100%;
  }
</style>
