<script lang="ts">
  // Action buttons of a change. The next step is the dark primary button.
  import type { Change } from '../../lib/api/types';
  import { actions, nextStep } from '../../lib/change/actions.svelte';
  import { session } from '../../lib/stores/me.svelte';
  import { router, seg } from '../../lib/router.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import Modal from '../../lib/components/Modal.svelte';
  import { mergePreview } from './trailers';

  let { c }: { c: Change } = $props();

  const step = $derived(nextStep(c));
  const busy = $derived(actions.busy[c.id]);
  const open = $derived(!['merged', 'deployed', 'verified', 'closed'].includes(c.state));
  const merged = $derived(['merged', 'deployed', 'verified'].includes(c.state));
  const isAgent = $derived(session.me?.kind !== 'human' && !!session.me);
  const stackSize = $derived(1 + (c.children?.length ?? 0));

  let confirmMerge = $state(false);
  let confirmClose = $state(false);
  let more = $state(false);

  const files = (q?: Record<string, string>) => {
    const s = new URLSearchParams(q).toString();
    router.navigate(`/changes/${seg(c.id)}/files${s ? '?' + s : ''}`);
  };

  async function copyPath() {
    if (!c.worktree) return;
    try {
      await navigator.clipboard.writeText(c.worktree);
    } catch {
      // no clipboard in this context
    }
  }
</script>

<div class="actions">
  {#if open}
    {#if c.dirty}
      <button class={['btn', step === 'commit' && 'dark']} onclick={() => files({ commit: '1' })}>
        <Icon name="commit" size={15} /> Commit
      </button>
    {/if}
    {#if c.behindTarget > 0 && c.local}
      <button class={['btn', step === 'rebase' && 'dark']} disabled={!!busy} onclick={() => actions.rebase(c.id)} title="git rebase origin/{c.target}, then push with lease">
        {#if busy === 'rebase'}<span class="spin"></span>{:else}<Icon name="rebase" size={15} />{/if}
        {stackSize > 1 ? `Rebase stack (${stackSize})` : `Rebase on ${c.parent ?? c.target}`}
      </button>
    {/if}
    {#if !c.local}
      <button class={['btn', step === 'checkout' && 'dark']} disabled={!!busy} onclick={() => actions.checkout(c.id)}>
        {#if busy === 'checkout'}<span class="spin"></span>{:else}<Icon name="folder" size={15} />{/if} Check out
      </button>
    {/if}
    {#if c.local}
      <button
        class={['btn', step === 'lab' && 'dark']}
        disabled={!!busy || c.labState === 'running'}
        onclick={() => actions.runLab(c.id)}
        title={c.dirty ? 'The lab tests the committed head' : 'Run the change lab on the head'}
      >
        {#if busy === 'lab' || c.labState === 'running'}<span class="spin"></span>{:else}<Icon name="flask" size={15} />{/if}
        {c.labState === 'running' ? 'Lab running' : c.labRuns.length ? 'Lab again' : 'Run lab'}
      </button>
    {/if}
    <a class="btn" href={`#/changes/${seg(c.id)}/review?advisor=risks`}><Icon name="sparkle" size={15} /> Ask advisor: risks</a>
    {#if c.local}
      <button class="btn" onclick={() => files()} title={c.worktree ? `${c.worktree} (click: open files, the path is copied)` : 'Open the files'} onclickcapture={copyPath}>
        <Icon name="folder" size={15} /> Open worktree
      </button>
    {/if}
    {#if c.local && (!c.pushed || c.aheadRemote > 0)}
      <button class={['btn', step === 'push' && 'dark']} disabled={!!busy} onclick={() => actions.push(c.id)}>
        {#if busy === 'push'}<span class="spin"></span>{:else}<Icon name="upload" size={15} />{/if}
        {c.pushed ? 'Push' : 'Push, open draft MR'}
      </button>
    {/if}
    {#if c.pushed && (c.mr?.draft || c.state === 'draft' || c.state === 'lab') && c.local}
      <button class={['btn', step === 'ready' && 'dark']} disabled={!!busy} onclick={() => actions.ready(c.id)}>
        {#if busy === 'ready'}<span class="spin"></span>{:else}<Icon name="check" size={15} />{/if} Mark ready
      </button>
    {/if}
    {#if c.group === 'review' || (c.state === 'review' && c.author !== session.me?.id)}
      <button class={['btn', step === 'approve' && 'dark']} disabled={!!busy || isAgent} onclick={() => actions.approve(c.id)}>
        {#if busy === 'approve'}<span class="spin"></span>{:else}<Icon name="check" size={15} />{/if} Approve
      </button>
    {/if}
    {#if c.state === 'review' || c.state === 'approved'}
      <button
        class={['btn', step === 'merge' && 'dark']}
        disabled={!!busy || isAgent}
        onclick={() => (confirmMerge = true)}
        title={c.gates?.mergeable ? 'Merge with a merge commit and trailers' : (c.gates?.blockers ?? []).join('; ')}
      >
        {#if busy === 'merge'}<span class="spin"></span>{:else}<Icon name="merge" size={15} />{/if} Merge
      </button>
    {/if}
  {/if}
  {#if merged}
    <button class="btn" disabled={!!busy} onclick={() => actions.revert(c.id)} title="Open a revert change (fast path)">
      {#if busy === 'revert'}<span class="spin"></span>{:else}<Icon name="rebase" size={15} />{/if} Revert
    </button>
  {/if}
  {#if open}
    <div class="more">
      <button class="btn ghost" aria-label="More actions" aria-expanded={more} onclick={() => (more = !more)}><Icon name="more" size={16} /></button>
      {#if more}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
        <div class="menu" onclick={() => (more = false)}>
          {#if c.mr}<a href={c.mr.url} target="_blank" rel="noreferrer"><Icon name="external" size={14} /> Open MR !{c.mr.number} in the forge</a>{/if}
          {#if c.worktree}<button onclick={copyPath}><Icon name="copy" size={14} /> Copy worktree path</button>{/if}
          <button class="danger" onclick={() => (confirmClose = true)}><Icon name="close" size={14} /> Close change</button>
        </div>
      {/if}
    </div>
  {/if}
</div>

<Modal bind:open={confirmMerge} title="Merge {c.id}" icon="merge" width={680}>
  {#if c.gates && !c.gates.mergeable}
    <div class="callout bad"><Icon name="warning" size={16} /><div><b>Not mergeable yet.</b> {(c.gates.blockers ?? []).join('; ')}</div></div>
  {/if}
  <p class="muted small">Merge commit on {c.target}, with the trailer block an auditor reads with <code>git log --merges</code>:</p>
  <pre class="preview">{mergePreview(c)}</pre>
  {#snippet footer()}
    <button class="btn" onclick={() => (confirmMerge = false)}>Cancel</button>
    <button
      class="btn dark"
      onclick={() => {
        confirmMerge = false;
        void actions.merge(c.id);
      }}
    >
      <Icon name="merge" size={15} /> Merge
    </button>
  {/snippet}
</Modal>

<Modal bind:open={confirmClose} title="Close {c.id}" icon="close" width={520}>
  <p>
    Close the change without merge.
    {c.mr ? `MR !${c.mr.number} is closed too and the branch stays on the forge.` : c.pushed ? 'The branch on the forge is deleted.' : ''}
    {c.local ? 'The local worktree is removed.' : ''}
  </p>
  {#if c.dirty}
    <div class="callout bad">
      <Icon name="warning" size={16} />
      <div><b>Uncommitted edits are discarded.</b> Commit them first to keep them in the branch.</div>
    </div>
  {/if}
  {#snippet footer()}
    <button class="btn" onclick={() => (confirmClose = false)}>Cancel</button>
    <button
      class="btn danger"
      onclick={() => {
        confirmClose = false;
        void actions.close(c.id);
      }}>Close change</button
    >
  {/snippet}
</Modal>

<style>
  .actions {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
    align-items: center;
  }
  .actions :global(.btn) {
    padding: 6px 13px;
    font-size: 13.5px;
    text-decoration: none;
  }
  .more {
    position: relative;
  }
  .menu {
    position: absolute;
    top: calc(100% + 4px);
    right: 0;
    z-index: 20;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius-s);
    box-shadow: var(--shadow);
    padding: 4px;
    display: grid;
    min-width: 240px;
  }
  .menu a,
  .menu button {
    display: flex;
    gap: 8px;
    align-items: center;
    padding: 6px 10px;
    border: 0;
    background: none;
    text-align: left;
    color: var(--ink);
    text-decoration: none;
    border-radius: 4px;
    font-size: 13px;
  }
  .menu a:hover,
  .menu button:hover {
    background: var(--panel-2);
  }
  .menu .danger {
    color: var(--bad);
  }
  .preview {
    font-size: 12.5px;
    white-space: pre-wrap;
  }
</style>
