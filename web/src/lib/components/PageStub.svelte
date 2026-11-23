<script lang="ts">
  // Placeholder for pages that phase 2 builds.
  import type { Snippet } from 'svelte';
  let {
    title,
    owner,
    note,
    children,
  }: { title: string; owner?: string; note?: string; children?: Snippet } = $props();
</script>

<section class="stub section">
  <h1>{title}</h1>
  {#if note}<p class="muted">{note}</p>{/if}
  {@render children?.()}
  {#if owner}<p class="muted small">Not built yet ({owner}).</p>{/if}
</section>

<style>
  .stub {
    display: grid;
    gap: 8px;
    margin: 16px;
  }
  p {
    margin: 0;
  }
</style>
