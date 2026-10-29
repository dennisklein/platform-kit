<script lang="ts">
  // A suggested edit (from chat or an advisor) as a diff with Accept/Reject.
  // Accept writes it to the worktree as a normal edit by the human.
  import { tick } from 'svelte';
  import type { Suggestion } from '../../api/ftypes';
  import { api, must, errorText } from '../../api/client';
  import { href } from '../../router.svelte';
  import { reducedMotion, suggestedBy } from '../../f/advisor';
  import UnifiedDiff from '../UnifiedDiff.svelte';
  import Glyph from '../Glyph.svelte';

  let {
    suggestion = $bindable(),
    onDecided,
    note = true,
  }: { suggestion: Suggestion; onDecided?: (s: Suggestion) => void; note?: boolean } = $props();

  let busy = $state<'accept' | 'reject' | null>(null);
  let error = $state<string | null>(null);
  let doneEl = $state<HTMLElement | null>(null);

  const file = $derived(suggestion.path.split('/').pop());
  const editor = $derived(href(`/changes/${suggestion.changeId}/files`, { path: suggestion.path }));
  const badge = $derived(suggestedBy(suggestion.by));

  async function decide(what: 'accept' | 'reject') {
    busy = what;
    error = null;
    try {
      const p = { params: { path: { sid: suggestion.id } } };
      const s = await must(
        what === 'accept' ? api.POST('/suggestions/{sid}/accept', p) : api.POST('/suggestions/{sid}/reject', p),
      );
      suggestion = s;
      onDecided?.(s);
      // Keep the outcome on screen ("Accepted by dennis …").
      await tick();
      doneEl?.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'nearest' });
    } catch (e) {
      error = errorText(e);
    } finally {
      busy = null;
    }
  }
</script>

<div class="sugg s-{suggestion.status}">
  <div class="head">
    <span class="badge"><Glyph name="sparkle" size={13} />{badge}</span>
    <span class="mono small muted">{suggestion.changeId}</span>
  </div>
  <div class="what">
    Edit in <a href={editor} title={suggestion.path}><strong>{file}</strong></a>{suggestion.title ? `: ${suggestion.title}` : ''}
  </div>
  <div class="diff"><UnifiedDiff diff={suggestion.diff} /></div>

  {#if suggestion.status === 'pending'}
    <div class="row">
      <button class="btn small accept" disabled={!!busy} onclick={() => decide('accept')}>
        <Glyph name="check" size={14} />{busy === 'accept' ? 'Accepting…' : 'Accept'}
      </button>
      <button class="btn small" disabled={!!busy} onclick={() => decide('reject')}>
        <Glyph name="x" size={13} />Reject
      </button>
      <a class="btn small ghost open" href={editor} title="Open {suggestion.path} in the editor">
        <Glyph name="external" size={13} />Open in editor
      </a>
    </div>
  {:else if suggestion.status === 'accepted'}
    <div class="done ok-text small" bind:this={doneEl}>
      <Glyph name="check" size={13} /> Accepted by {suggestion.decidedBy ?? 'you'}. Now it is a normal edit, owned by
      {suggestion.decidedBy ?? 'you'}.
      <a href={editor}>Open in editor</a>
    </div>
  {:else if suggestion.status === 'rejected'}
    <div class="done muted small" bind:this={doneEl}>
      <Glyph name="x" size={13} /> Rejected{suggestion.decidedBy ? ` by ${suggestion.decidedBy}` : ''}.
    </div>
  {:else}
    <div class="done warn-text small">Stale: the file changed after the suggestion. Ask again.</div>
  {/if}
  {#if error}<div class="error-box">{error}</div>{/if}
  {#if note && suggestion.status === 'pending'}
    <p class="muted small note">Chat does not commit, run labs or mark ready on its own.</p>
  {/if}
</div>

<style>
  .sugg {
    display: grid;
    gap: 6px;
    min-width: 0;
  }
  .head {
    display: flex;
    justify-content: space-between;
    gap: 8px;
    align-items: center;
    font-size: 13px;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font: 600 12px var(--sans);
    color: var(--ai);
    background: var(--ai-soft);
    border: 1px solid var(--ai-line);
    border-radius: 999px;
    padding: 1px 9px;
    white-space: nowrap;
  }
  .what {
    font-size: 13.5px;
    color: var(--muted);
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .what a {
    color: var(--ink);
  }
  .diff {
    border-left: 3px solid var(--ai-fill);
    background: var(--code-bg);
    border-radius: 0 var(--radius-s) var(--radius-s) 0;
    min-width: 0;
  }
  .diff :global(pre) {
    border: 0;
    background: transparent;
  }
  .row {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .row .btn {
    padding: 4px 12px;
    font-size: 13px;
  }
  /* The one-click apply: the strongest button of the card, in both themes. */
  .accept {
    background: var(--ai-fill);
    border-color: var(--ai-fill);
    color: var(--ai-on);
    font-weight: 700;
  }
  .accept:hover:not(:disabled) {
    background: color-mix(in srgb, var(--ai-fill) 85%, black) !important;
    border-color: color-mix(in srgb, var(--ai-fill) 85%, black) !important;
    color: var(--ai-on);
  }
  .open {
    margin-left: auto;
    color: var(--ai);
  }
  .done {
    display: flex;
    gap: 6px;
    align-items: baseline;
    flex-wrap: wrap;
    scroll-margin-bottom: 24px;
  }
  .s-accepted .diff {
    border-left-color: var(--ok);
  }
  .s-rejected .diff,
  .s-stale .diff {
    border-left-color: var(--line-strong);
    opacity: 0.65;
  }
  .note {
    margin: 0;
  }
</style>
