<script lang="ts">
  // New change: title, intent, target (main or stacked on an open change),
  // work items. The bench makes worktree, branch and metadata file.
  import { api, must, errorText } from '../../lib/api/client';
  import type { WorkItem } from '../../lib/api/types';
  import { changes } from '../../lib/stores/changes.svelte';
  import { toasts } from '../../lib/stores/toasts.svelte';
  import { router, seg } from '../../lib/router.svelte';
  import Modal from '../../lib/components/Modal.svelte';

  let { open = $bindable(false) }: { open?: boolean } = $props();

  let title = $state('');
  let intent = $state('');
  let stack = $state(false);
  let parent = $state('');
  let workItems = $state<string[]>([]);
  let items = $state<WorkItem[]>([]);
  let busy = $state(false);
  let error = $state<string | null>(null);

  const candidates = $derived(changes.list.filter((c) => c.pushed || c.local));
  const parentChange = $derived(changes.byId(parent));
  const slug = $derived(
    title
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 40)
      .replace(/-+$/, '') || 'title',
  );
  const valid = $derived(title.trim().length >= 3 && (!stack || !!parentChange));

  $effect(() => {
    if (!open) return;
    error = null;
    void (async () => {
      try {
        const b = await must(api.GET('/work'));
        items = b.items.filter((i) => i.status !== 'done');
      } catch {
        items = [];
      }
    })();
  });

  $effect(() => {
    if (stack && !parent && candidates.length) parent = candidates[0].id;
  });

  function reset() {
    title = '';
    intent = '';
    stack = false;
    parent = '';
    workItems = [];
  }

  async function create(e?: Event) {
    e?.preventDefault();
    if (!valid || busy) return;
    busy = true;
    error = null;
    try {
      const c = await must(
        api.POST('/changes', {
          body: {
            title: title.trim(),
            intent: intent.trim() || undefined,
            target: stack && parentChange ? parentChange.branch : 'main',
            parent: stack && parentChange ? parentChange.id : undefined,
            workItems: workItems.length ? workItems : undefined,
          },
        }),
      );
      toasts.ok(`Change ${c.id} created.`, `Branch ${c.branch}${c.worktree ? `, worktree ${c.worktree}` : ''}.`);
      open = false;
      reset();
      changes.later(0);
      router.navigate(`/changes/${seg(c.id)}/files?path=${encodeURIComponent(`.pk/changes/${c.id}.yaml`)}`);
    } catch (err) {
      error = errorText(err);
    } finally {
      busy = false;
    }
  }

  function toggleItem(id: string) {
    workItems = workItems.includes(id) ? workItems.filter((x) => x !== id) : [...workItems, id];
  }
</script>

<Modal bind:open title="New change" icon="branch" width={720}>
  <form id="new-change" class="form" onsubmit={create}>
    <label for="nc-title">Title</label>
    <input id="nc-title" class="input" bind:value={title} placeholder="What changes, in a few words" maxlength="120" required />

    <label for="nc-intent">Intent</label>
    <input id="nc-intent" class="input" bind:value={intent} placeholder="Why: the intent that reviewers and auditors read" />

    <span class="lab">Target</span>
    <div class="row">
      <div class="seg" role="radiogroup" aria-label="Target">
        <button type="button" role="radio" aria-checked={!stack} onclick={() => (stack = false)}>main</button>
        <button type="button" role="radio" aria-checked={stack} onclick={() => (stack = true)} disabled={!candidates.length}>
          stack on {parentChange?.id ?? 'a change'}
        </button>
      </div>
      {#if stack}
        <select class="select" bind:value={parent} aria-label="Parent change">
          {#each candidates as c (c.id)}
            <option value={c.id}>{c.id} · {c.title}</option>
          {/each}
        </select>
      {/if}
    </div>

    <span class="lab">Work item</span>
    <div class="items">
      {#each items as w (w.id)}
        <button type="button" class="wi" aria-pressed={workItems.includes(w.id)} onclick={() => toggleItem(w.id)} title={w.title}>
          <span class="mono">{w.id}</span> {w.title}
        </button>
      {:else}
        <span class="muted small">No open work items.</span>
      {/each}
    </div>

    <div class="info">
      Makes a worktree, the branch <code>change/c-······-{slug}</code>
      {#if stack && parentChange}on <code>{parentChange.branch}</code>{/if}
      and <code>.pk/changes/c-······.yaml</code>.
    </div>

    {#if error}<div class="error-box full">{error}</div>{/if}
  </form>

  {#snippet footer()}
    <button class="btn" onclick={() => (open = false)}>Cancel</button>
    <button class="btn dark" type="submit" form="new-change" disabled={!valid || busy}>
      {busy ? 'Creating…' : 'Create change'}
    </button>
  {/snippet}
</Modal>

<style>
  /* Title and intent are prose: typed in the sans font, placeholders in
     the muted colour. */
  .form :global(.input::placeholder) {
    color: var(--muted);
    opacity: 0.8;
  }
  .form {
    display: grid;
    grid-template-columns: 110px minmax(0, 1fr);
    gap: 12px 14px;
    align-items: center;
  }
  label,
  .lab {
    color: var(--ink);
    font-size: 14px;
  }
  .input {
    padding: 8px 12px;
    font-size: 14px;
  }
  .seg {
    display: inline-flex;
    gap: 6px;
  }
  .seg button {
    font: 13px var(--mono);
    border: 1px solid var(--line-strong);
    background: var(--panel);
    border-radius: 5px;
    padding: 5px 12px;
  }
  .seg button[aria-checked='true'] {
    background: var(--strong-fill);
    border-color: var(--strong-fill);
    color: var(--strong-on);
  }
  .items {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .wi {
    font-size: 12.5px;
    border: 1px solid var(--line-strong);
    background: var(--panel);
    border-radius: 5px;
    padding: 3px 9px;
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .wi[aria-pressed='true'] {
    background: var(--accent-soft);
    border-color: var(--accent-fill);
  }
  .info {
    grid-column: 1 / -1;
    background: var(--panel-2);
    border: 1px solid var(--line);
    border-radius: var(--radius-s);
    padding: 10px 14px;
    color: var(--muted);
    font-size: 13.5px;
    line-height: 1.7;
  }
  .info code {
    color: var(--ink);
  }
  .full {
    grid-column: 1 / -1;
  }
</style>
