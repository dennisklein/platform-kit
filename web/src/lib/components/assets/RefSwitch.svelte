<script lang="ts">
  // Pick the state to show: main or the change you work on (?ref=).
  import { router } from '../../router.svelte';

  let { value, changeId }: { value: string; changeId?: string } = $props();

  function pick(ref: string) {
    const q = new URLSearchParams(router.query);
    if (ref === 'main') q.delete('ref');
    else q.set('ref', ref);
    q.set('pinref', '1');
    const s = q.toString();
    router.navigate(router.path + (s ? `?${s}` : ''));
  }

  const opts = $derived([
    'main',
    ...(changeId && changeId !== 'main' ? [changeId] : []),
    ...(value !== 'main' && value !== changeId ? [value] : []),
  ]);
</script>

{#if opts.length > 1}
  <div class="seg" role="group" aria-label="State">
    {#each opts as o (o)}
      <button class="mono" aria-pressed={value === o} onclick={() => pick(o)}>{o}</button>
    {/each}
  </div>
{/if}

<style>
  .seg {
    display: inline-flex;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    overflow: hidden;
  }
  button {
    border: 0;
    background: var(--panel);
    padding: 3px 10px;
    font-size: 12px;
    color: var(--muted);
  }
  button + button {
    border-left: 1px solid var(--line-strong);
  }
  button[aria-pressed='true'] {
    background: var(--gsi-navy);
    color: #fff;
  }
</style>
