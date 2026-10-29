<script lang="ts">
  // New file from a kind (skeleton from schema defaults at the kind's path),
  // or a plain file by path.
  import { api, must, errorText } from '../../lib/api/client';
  import type { Kind } from '../../lib/api/types';
  import Modal from '../../lib/components/Modal.svelte';

  let {
    open = $bindable(false),
    changeId,
    kinds,
    oncreated,
  }: { open?: boolean; changeId: string; kinds: Kind[]; oncreated: (path: string) => void } = $props();

  let kind = $state('Node');
  let name = $state('');
  let path = $state('');
  let busy = $state(false);
  let error = $state<string | null>(null);

  const creatable = $derived(kinds.filter((k) => k.own || (k.hooks ?? []).includes('skeleton')).sort((a, b) => a.name.localeCompare(b.name)));
  const k = $derived(kinds.find((x) => x.name === kind));
  const plain = $derived(kind === '');
  const valid = $derived(plain ? /^[\w.-]+(\/[\w.-]+)*$/.test(path.trim()) : /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(name.trim()));

  async function create(e?: Event) {
    e?.preventDefault();
    if (!valid || busy) return;
    busy = true;
    error = null;
    try {
      if (plain) {
        const p = path.trim();
        await must(api.PUT('/changes/{id}/file', { params: { path: { id: changeId }, query: { path: p } }, body: { content: '' } }));
        oncreated(p);
      } else {
        const f = await must(api.POST('/changes/{id}/file/new', { params: { path: { id: changeId } }, body: { kind, name: name.trim() } }));
        oncreated(f.path);
      }
      open = false;
      name = '';
      path = '';
    } catch (err) {
      error = errorText(err);
    } finally {
      busy = false;
    }
  }
</script>

<Modal bind:open title="New file" icon="file" width={560}>
  <form id="new-file" class="form" onsubmit={create}>
    <label for="nf-kind">Kind</label>
    <select id="nf-kind" class="select" bind:value={kind}>
      {#each creatable as x (x.name)}<option value={x.name}>{x.name} · {x.apiVersion}</option>{/each}
      <option value="">Plain file (path)</option>
    </select>
    {#if plain}
      <label for="nf-path">Path</label>
      <input id="nf-path" class="input mono" bind:value={path} placeholder="docs/capacity/defiant.md" />
    {:else}
      <label for="nf-name">Name</label>
      <input id="nf-name" class="input mono" bind:value={name} placeholder={kind === 'Node' ? 'cn-035' : 'name'} />
      {#if k}
        <p class="hint">Path from the kind: <code>{(k.paths[0] ?? '').replace('*', name || '<name>')}</code>. The skeleton has the schema defaults; the form shows what is missing.</p>
      {/if}
    {/if}
    {#if error}<div class="error-box full">{error}</div>{/if}
  </form>
  {#snippet footer()}
    <button class="btn" onclick={() => (open = false)}>Cancel</button>
    <button class="btn dark" type="submit" form="new-file" disabled={!valid || busy}>{busy ? 'Creating…' : 'Create file'}</button>
  {/snippet}
</Modal>

<style>
  .form {
    display: grid;
    grid-template-columns: 70px minmax(0, 1fr);
    gap: 12px;
    align-items: center;
  }
  .input,
  .select {
    padding: 7px 10px;
  }
  .hint {
    grid-column: 2;
    margin: 0;
    color: var(--muted);
    font-size: 12.5px;
  }
  .full {
    grid-column: 1 / -1;
  }
</style>
