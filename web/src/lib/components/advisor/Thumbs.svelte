<script lang="ts">
  // Thumbs up/down for an advisor result or chat answer; the rating goes to
  // the LLM log entry (with the model name).
  import { api, must, errorText } from '../../api/client';
  import Glyph from '../Glyph.svelte';

  let {
    logId,
    rating = $bindable(),
    note = true,
    compact = false,
  }: { logId?: string; rating?: 'up' | 'down'; note?: boolean; compact?: boolean } = $props();

  let busy = $state(false);
  let error = $state<string | null>(null);

  async function rate(r: 'up' | 'down') {
    if (!logId || busy) return;
    busy = true;
    error = null;
    const before = rating;
    rating = r;
    try {
      await must(api.POST('/llm/log/{id}/rating', { params: { path: { id: logId } }, body: { rating: r } }));
    } catch (e) {
      rating = before;
      error = errorText(e);
    } finally {
      busy = false;
    }
  }
</script>

<div class="thumbs" class:compact>
  <button
    class="t up"
    class:on={rating === 'up'}
    aria-pressed={rating === 'up'}
    disabled={!logId || busy}
    onclick={() => rate('up')}
    title="Helpful"
  >
    <Glyph name="thumbs-up" size={compact ? 13 : 15} /><span class="sr-only">Thumbs up</span>
  </button>
  <button
    class="t down"
    class:on={rating === 'down'}
    aria-pressed={rating === 'down'}
    disabled={!logId || busy}
    onclick={() => rate('down')}
    title="Not helpful"
  >
    <Glyph name="thumbs-down" size={compact ? 13 : 15} /><span class="sr-only">Thumbs down</span>
  </button>
  {#if error}
    <span class="small bad-text">{error}</span>
  {:else if note && rating}
    <span class="small muted">Rated. Saved in the LLM log with the model name.</span>
  {/if}
</div>

<style>
  .thumbs {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .t {
    display: inline-grid;
    place-items: center;
    width: 30px;
    height: 28px;
    border-radius: 6px;
    border: 1px solid var(--line-strong);
    background: var(--panel);
    color: var(--muted);
  }
  .compact .t {
    width: 24px;
    height: 22px;
    border-color: transparent;
    background: transparent;
  }
  .t:hover:not(:disabled) {
    color: var(--ink);
    border-color: var(--grey-400);
  }
  .up.on {
    background: var(--ok);
    border-color: var(--ok);
    color: #fff;
  }
  .down.on {
    background: var(--bad);
    border-color: var(--bad);
    color: #fff;
  }
  .t:disabled {
    cursor: default;
  }
</style>
