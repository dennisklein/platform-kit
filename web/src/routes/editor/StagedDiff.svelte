<script lang="ts">
  // What the next commit changes: each selected file of the worktree
  // against the change head (secrets are masked on both sides).
  import { api, must } from '../../lib/api/client';
  import { collapse, diffStats, lineDiff, type DiffLine } from '../../lib/change/diff';
  import { baseName } from '../../lib/change/format';
  import { href } from '../../lib/router.svelte';
  import Icon from '../../lib/components/Icon.svelte';

  let { changeId, head, paths }: { changeId: string; head?: string; paths: string[] } = $props();

  type Row = DiffLine | { kind: 'gap'; count: number };
  interface FileDiff {
    path: string;
    status: 'added' | 'deleted' | 'modified' | 'same';
    rows: Row[];
    added: number;
    deleted: number;
  }

  let files = $state<FileDiff[]>([]);
  let loading = $state(false);
  let closed = $state(new Set<string>());
  let seq = 0;

  async function read(p: string): Promise<{ exists: boolean; content: string }> {
    try {
      const f = await must(api.GET('/changes/{id}/file', { params: { path: { id: changeId }, query: { path: p } } }));
      return { exists: f.exists !== false, content: f.content };
    } catch {
      return { exists: false, content: '' };
    }
  }

  async function readHead(p: string): Promise<{ exists: boolean; content: string }> {
    if (!head) return { exists: false, content: '' };
    try {
      const f = await must(api.GET('/refs/{ref}/file', { params: { path: { ref: head }, query: { path: p, convert: false } } }));
      return { exists: f.exists !== false, content: f.content };
    } catch {
      return { exists: false, content: '' };
    }
  }

  async function load(ps: string[]) {
    const n = ++seq;
    loading = true;
    const out = await Promise.all(
      ps.map(async (p): Promise<FileDiff> => {
        const [a, b] = await Promise.all([readHead(p), read(p)]);
        const split = (t: string) => t.replace(/\n$/, '').split('\n');
        let lines: DiffLine[];
        if (!a.exists) lines = b.exists ? split(b.content).map((text, i) => ({ kind: 'add' as const, text, new: i + 1 })) : [];
        else if (!b.exists) lines = split(a.content).map((text, i) => ({ kind: 'del' as const, text, old: i + 1 }));
        else lines = lineDiff(a.content.replace(/\n$/, ''), b.content.replace(/\n$/, ''));
        const st = diffStats(lines);
        const status = !a.exists ? 'added' : !b.exists ? 'deleted' : st.added + st.deleted ? 'modified' : 'same';
        const rows = status === 'modified' ? collapse(lines, 3) : lines;
        return { path: p, status, rows, added: st.added, deleted: st.deleted };
      }),
    );
    if (n !== seq) return;
    files = out;
    loading = false;
  }

  $effect(() => {
    const key = [...paths].sort();
    void head;
    void load(key);
  });

  function toggle(p: string) {
    const s = new Set(closed);
    if (s.has(p)) s.delete(p);
    else s.add(p);
    closed = s;
  }

  const FLAG = { added: '+', deleted: '−', modified: 'M', same: '=' } as const;
</script>

<section class="card staged">
  <div class="head">
    <h3 class="eyebrow">Staged diff</h3>
    <span class="muted small">{paths.length ? `${paths.length} file${paths.length === 1 ? '' : 's'} against the change head` : 'select files on the left'}</span>
  </div>
  {#if loading && !files.length}
    <p class="muted small">Loading the diff…</p>
  {/if}
  {#each files as f (f.path)}
    <div class="file">
      <button class="fhead" onclick={() => toggle(f.path)} aria-expanded={!closed.has(f.path)}>
        <Icon name={closed.has(f.path) ? 'chevron-right' : 'chevron-down'} size={14} />
        <span class="flag f-{f.status}">{FLAG[f.status]}</span>
        <span class="mono name" title={f.path}>{baseName(f.path)}</span>
        <span class="mono muted small dir">{f.path.slice(0, -baseName(f.path).length)}</span>
        <span class="spacer"></span>
        <span class="mono small"><span class="add">+{f.added}</span> <span class="del">−{f.deleted}</span></span>
        <a class="small" href={href(`/changes/${changeId}/files`, { path: f.path })} onclick={(e) => e.stopPropagation()}>open</a>
      </button>
      {#if !closed.has(f.path)}
        {#if f.status === 'same'}
          <p class="muted small pad">No text change (only masked values or whitespace).</p>
        {:else}
          <pre class="udiff">{#each f.rows as r, i (i)}{#if r.kind === 'gap'}<span class="ln gap">⋯ {r.count} unchanged line{r.count === 1 ? '' : 's'}</span>{:else}<span class="ln k-{r.kind}"><span class="no">{r.kind === 'add' ? '' : (r.old ?? '')}</span><span class="no">{r.kind === 'del' ? '' : (r.new ?? '')}</span><span class="mark">{r.kind === 'add' ? '+' : r.kind === 'del' ? '−' : ' '}</span>{r.text || ' '}</span>{/if}{/each}</pre>
        {/if}
      {/if}
    </div>
  {/each}
</section>

<style>
  .staged {
    display: grid;
    gap: 10px;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 12px;
  }
  .file {
    border: 1px solid var(--line);
    border-radius: 6px;
    overflow: hidden;
  }
  .fhead {
    display: flex;
    align-items: center;
    gap: 8px;
    width: 100%;
    border: 0;
    background: var(--panel-2);
    color: var(--ink);
    padding: 7px 12px;
    font-size: 13.5px;
    text-align: left;
  }
  .name {
    font-weight: 600;
  }
  .dir {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .spacer {
    flex: 1;
  }
  .flag {
    font: 700 12px var(--mono);
    width: 12px;
    text-align: center;
  }
  .f-added,
  .add {
    color: var(--ok);
  }
  .f-deleted,
  .del {
    color: var(--bad);
  }
  .f-modified {
    color: var(--accent);
  }
  .pad {
    margin: 0;
    padding: 8px 12px;
  }
  .udiff {
    margin: 0;
    padding: 4px 0;
    font-size: 12.5px;
    line-height: 1.55;
    overflow-x: auto;
    max-height: 420px;
    overflow-y: auto;
    border-top: 1px solid var(--line);
    border-radius: 0;
  }
  .ln {
    display: block;
    white-space: pre;
    min-width: max-content;
    padding-right: 12px;
  }
  .no {
    display: inline-block;
    width: 34px;
    text-align: right;
    padding-right: 6px;
    color: var(--muted);
    user-select: none;
  }
  .mark {
    display: inline-block;
    width: 18px;
    text-align: center;
    color: var(--muted);
    user-select: none;
  }
  .k-add {
    background: var(--ok-soft);
  }
  .k-add .mark {
    color: var(--ok);
  }
  .k-del {
    background: var(--bad-soft);
  }
  .k-del .mark {
    color: var(--bad);
  }
  .gap {
    color: var(--muted);
    padding-left: 12px;
    background: var(--panel-2);
  }
</style>
