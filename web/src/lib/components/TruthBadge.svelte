<script lang="ts">
  // Truth class of a field (intent, asserted, derived, ...) and its owner system.
  let { truth, owner }: { truth?: string; owner?: string } = $props();

  const OWNERS: Record<string, string> = { 'asset-db': 'asset DB', people: 'people DB' };
  const ownerLabel = $derived(owner ? (OWNERS[owner] ?? owner) : '');
  const title = $derived(
    [truth ? `truth class: ${truth}` : '', owner ? `owner: ${ownerLabel} (synced, not edited by hand)` : '']
      .filter(Boolean)
      .join(' · '),
  );
</script>

<span class="tb t-{truth ?? 'none'}" class:owned={!!owner} {title}>{ownerLabel || truth || '—'}</span>

<style>
  .tb {
    display: inline-block;
    font: 500 11px/1.6 var(--mono);
    padding: 0 6px;
    border-radius: 4px;
    white-space: nowrap;
    background: var(--line);
    color: var(--muted);
  }
  .t-intent {
    background: var(--accent-soft);
    color: var(--accent);
  }
  .t-asserted,
  .owned {
    background: var(--info-soft);
    color: var(--info);
  }
  .t-generated {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .t-imperative-only {
    background: var(--bad-soft);
    color: var(--bad);
  }
</style>
