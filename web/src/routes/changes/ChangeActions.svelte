<script lang="ts">
  // Action buttons of a change. The next step is the dark primary button.
  import type { Change } from '../../lib/api/types';
  import { actions, nextStep } from '../../lib/change/actions.svelte';
  import { session } from '../../lib/stores/me.svelte';
  import { router, seg } from '../../lib/router.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import Modal from '../../lib/components/Modal.svelte';
  import { tick } from 'svelte';
  import { mergeParts } from './trailers';
  import TrailerBlock from './TrailerBlock.svelte';

  let { c }: { c: Change } = $props();

  // Merge waits for the gates. The button stays clickable: its dialog says why.
  const blocked = $derived(!!c.gates && !c.gates.mergeable);
  const blockers = $derived(c.gates?.blockers ?? []);
  const approvalsLeft = $derived(
    c.gates && c.gates.approvals.have < c.gates.approvals.need ? `${c.gates.approvals.have} of ${c.gates.approvals.need} approvals` : '',
  );
  const impactText = $derived(c.impact && c.impact.added + c.impact.changed + c.impact.deleted > 0 ? c.impact.text : '');

  const step = $derived(nextStep(c));
  const busy = $derived(actions.busy[c.id]);
  const open = $derived(!['merged', 'deployed', 'verified', 'closed'].includes(c.state));
  const merged = $derived(['merged', 'deployed', 'verified'].includes(c.state));
  const isAgent = $derived(session.me?.kind !== 'human' && !!session.me);
  const stackSize = $derived(1 + (c.children?.length ?? 0));

  let confirmMerge = $state(false);
  let confirmClose = $state(false);
  let more = $state(false);

  // One row, never wrapped on a wide screen: when the buttons do not fit,
  // the secondary ones move into the "…" menu, least needed first. The
  // next step (the dark button) never moves.
  type Secondary = 'worktree' | 'lab' | 'impact';
  const secondary = $derived<Secondary[]>(
    open
      ? [
          ...(c.local ? (['worktree'] as const) : []),
          ...(c.local && step !== 'lab' ? (['lab'] as const) : []),
          ...(impactText ? (['impact'] as const) : []),
        ]
      : [],
  );
  let level = $state(0);
  let wrap = $state(false);
  let row = $state<HTMLElement | null>(null);
  const moved = (k: Secondary) => secondary.slice(0, level).includes(k);

  async function fit() {
    if (!row) return;
    level = 0;
    wrap = false;
    await tick();
    while (row && row.scrollWidth > row.clientWidth + 1 && level < secondary.length) {
      level++;
      await tick();
    }
    if (row && row.scrollWidth > row.clientWidth + 1) wrap = true;
  }

  $effect(() => {
    // Refit when the buttons change (state, busy, lab) or the row resizes.
    void secondary.length;
    void step;
    void busy;
    void c.state;
    void c.behindTarget;
    void c.dirty;
    void c.labState;
    void fit();
  });
  $effect(() => {
    if (!row) return;
    let w = row.clientWidth;
    const ro = new ResizeObserver(() => {
      if (row && row.clientWidth !== w) {
        w = row.clientWidth;
        void fit();
      }
    });
    ro.observe(row);
    return () => ro.disconnect();
  });

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

<div class="actions" class:wrap bind:this={row}>
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
    {#if c.local && !moved('lab')}
      <button
        class={['btn', step === 'lab' && 'dark']}
        disabled={!!busy || c.labState === 'running'}
        onclick={() => actions.runLab(c.id)}
        title={c.dirty ? 'The lab tests the committed head' : 'Run the change lab on the head'}
      >
        {#if busy === 'lab' || c.labState === 'running'}<span class="spin"></span>{:else}<Icon name="flask" size={15} />{/if}
        {c.labState === 'running' ? 'Lab running' : c.labRuns.length ? 'Run lab again' : 'Run lab'}
      </button>
    {/if}
    {#if impactText && !moved('impact')}
      <a class="btn impact" href={`#/changes/${seg(c.id)}/review`} title="What the change does to the rendered clusters">
        <Icon name="hexagon" size={15} /> Impact: <span class="mono">{impactText}</span>
      </a>
    {/if}
    <a class="btn ai" href={`#/changes/${seg(c.id)}/review?advisor=risks`}><Icon name="sparkle" size={15} /> Ask the advisor about risks</a>
    {#if c.local && !moved('worktree')}
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
      <!-- Blocked: still clickable, the dialog tells why. -->
      <button
        class={['btn', step === 'merge' && 'dark', blocked && 'blocked']}
        disabled={!!busy || isAgent}
        aria-disabled={blocked}
        onclick={() => (confirmMerge = true)}
        title={blocked ? `Not mergeable yet: ${blockers.join('; ')}` : 'Merge with a merge commit and trailers'}
      >
        {#if busy === 'merge'}<span class="spin"></span>{:else}<Icon name={blocked ? 'lock' : 'merge'} size={15} />{/if} Merge
        {#if blocked && approvalsLeft}<span class="why">{approvalsLeft}</span>{/if}
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
          {#if moved('lab')}
            <button disabled={!!busy || c.labState === 'running'} onclick={() => actions.runLab(c.id)}>
              <Icon name="flask" size={14} /> {c.labState === 'running' ? 'Lab running' : c.labRuns.length ? 'Run lab again' : 'Run lab'}
            </button>
          {/if}
          {#if moved('impact')}
            <a href={`#/changes/${seg(c.id)}/review`}><Icon name="hexagon" size={14} /> Impact: <span class="mono">{impactText}</span></a>
          {/if}
          {#if moved('worktree')}
            <button onclick={() => files()} onclickcapture={copyPath}><Icon name="folder" size={14} /> Open worktree</button>
          {/if}
          {#if c.mr}<a href={c.mr.url} target="_blank" rel="noreferrer"><Icon name="external" size={14} /> Open MR !{c.mr.number} in the forge</a>{/if}
          {#if c.worktree}<button onclick={copyPath}><Icon name="copy" size={14} /> Copy worktree path</button>{/if}
          <button class="danger" onclick={() => (confirmClose = true)}><Icon name="close" size={14} /> Close change</button>
        </div>
      {/if}
    </div>
  {/if}
</div>

<Modal bind:open={confirmMerge} title="Merge {c.id}" icon="merge" width={760}>
  {#if blocked}
    <div class="callout bad">
      <Icon name="lock" size={16} />
      <div>
        <b>Not mergeable yet.</b>
        {#if blockers.length === 1}{blockers[0]}.{:else}
          <ul class="blockers">{#each blockers as b (b)}<li>{b}</li>{/each}</ul>
        {/if}
      </div>
    </div>
  {/if}
  <p class="muted small">Merge commit on {c.target}, with the trailer block an auditor reads with <code>git log --merges</code>:</p>
  <TrailerBlock parts={mergeParts(c)} />
  {#snippet footer()}
    <button class="btn" onclick={() => (confirmMerge = false)}>Cancel</button>
    <button
      class="btn dark"
      disabled={blocked}
      onclick={() => {
        confirmMerge = false;
        void actions.merge(c.id);
      }}
    >
      <Icon name={blocked ? 'lock' : 'merge'} size={15} />
      {blocked ? 'Merge (blocked)' : 'Merge'}
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
    flex-wrap: nowrap;
    align-items: center;
    min-width: 0;
  }
  .actions > :global(*) {
    flex: none;
  }
  /* Narrow (side by side with a terminal): wrap when even the short row does not fit. */
  .actions.wrap {
    flex-wrap: wrap;
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
  .blockers {
    margin: 4px 0 0;
    padding-left: 18px;
  }
  /* Merge before the gates are met: visible, but plainly not the next step. */
  .actions :global(.btn.blocked) {
    border-style: dashed;
    color: var(--muted);
    background: transparent;
  }
  .why {
    font-size: 12px;
    font-weight: 500;
    color: var(--muted);
    padding-left: 6px;
    margin-left: 2px;
    border-left: 1px solid var(--line-strong);
  }
  .actions :global(.btn.impact) {
    border-color: color-mix(in srgb, var(--accent-fill) 55%, var(--line));
  }
  .impact .mono {
    font-weight: 600;
  }
</style>
