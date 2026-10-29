<script lang="ts">
  // A suggested edit (from chat or an advisor) as a diff with Accept/Reject.
  // Accept writes it to the worktree as a normal edit by the human.
  import type { Suggestion } from '../../api/ftypes';
  import { api, must, errorText } from '../../api/client';
  import { href } from '../../router.svelte';
  import UnifiedDiff from '../UnifiedDiff.svelte';
  import Glyph from '../Glyph.svelte';

  let {
    suggestion = $bindable(),
    onDecided,
    note = true,
  }: { suggestion: Suggestion; onDecided?: (s: Suggestion) => void; note?: boolean } = $props();

  let busy = $state<'accept' | 'reject' | null>(null);
  let error = $state<string | null>(null);

  const file = $derived(suggestion.path.split('/').pop());
  const editor = $derived(href(`/changes/${suggestion.changeId}/files`, { path: suggestion.path }));

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
    } catch (e) {
      error = errorText(e);
    } finally {
      busy = null;
    }
  }
</script>

<div class="sugg s-{suggestion.status}">
  <div class="head">
    <span>Suggested edit in <a href={editor} title={suggestion.path}><strong>{file}</strong></a>:</span>
    <span class="mono small muted">{suggestion.changeId}</span>
  </div>
  {#if suggestion.title}<div class="title small">{suggestion.title}</div>{/if}
  <div class="diff"><UnifiedDiff diff={suggestion.diff} /></div>

  {#if suggestion.status === 'pending'}
    <div class="row">
      <button class="btn small accept" disabled={!!busy} onclick={() => decide('accept')}>
        <Glyph name="check" size={13} />{busy === 'accept' ? 'Accepting…' : 'Accept'}
      </button>
      <button class="btn small" disabled={!!busy} onclick={() => decide('reject')}>
        <Glyph name="x" size={13} />Reject
      </button>
    </div>
  {:else if suggestion.status === 'accepted'}
    <div class="done ok-text small">
      <Glyph name="check" size={13} /> Accepted by {suggestion.decidedBy ?? 'you'}. Now it is a normal edit, owned by
      {suggestion.decidedBy ?? 'you'}.
      <a href={editor}>Open in editor</a>
    </div>
  {:else if suggestion.status === 'rejected'}
    <div class="done muted small"><Glyph name="x" size={13} /> Rejected{suggestion.decidedBy ? ` by ${suggestion.decidedBy}` : ''}.</div>
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
    align-items: baseline;
    font-size: 13px;
  }
  .head a {
    color: inherit;
  }
  .title {
    color: var(--muted);
  }
  .diff {
    border-left: 3px solid var(--accent-fill);
    background: var(--code-bg);
    border-radius: 0 var(--radius-s) var(--radius-s) 0;
    min-width: 0;
  }
  .diff :global(pre) {
    border: 0;
    background: transparent;
  }
  .accept {
    background: var(--gsi-navy);
    border-color: var(--gsi-navy);
    color: #fff;
    font-weight: 600;
  }
  .accept:hover:not(:disabled) {
    background: var(--navy-900) !important;
    color: #fff;
  }
  .done {
    display: flex;
    gap: 6px;
    align-items: baseline;
    flex-wrap: wrap;
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
