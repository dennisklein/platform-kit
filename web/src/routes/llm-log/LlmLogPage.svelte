<script lang="ts">
  // LLM log: every model call with its redacted prompt, answer, sources and
  // rating. Search, rate, export (JSON lines) and delete with confirmation.
  import type { Params } from '../../lib/router.svelte';
  import { router, href } from '../../lib/router.svelte';
  import { api, must, errorText, ApiError } from '../../lib/api/client';
  import type { LlmLogEntry } from '../../lib/api/ftypes';
  import { toasts } from '../../lib/stores/toasts.svelte';
  import { dateTime, ago, duration, isChangeId } from '../../lib/f/fmt';
  import SourceChip from '../../lib/components/advisor/SourceChip.svelte';
  import Thumbs from '../../lib/components/advisor/Thumbs.svelte';
  import Markdown from '../../lib/components/Markdown.svelte';
  import Modal from '../../lib/components/Modal.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  type Surface = LlmLogEntry['surface'];
  const SURFACES: [Surface, string][] = [
    ['chat', 'chat'],
    ['advisor', 'advisor'],
    ['commit-message', 'commit message'],
    ['mcp', 'MCP'],
  ];

  let entries = $state<LlmLogEntry[]>([]);
  let total = $state(0);
  let loaded = $state(false);
  let error = $state<string | null>(null);
  let text = $state('');
  let surface = $state<Surface | ''>('');
  let rated = $state<'' | 'up' | 'down'>('');
  let open = $state<Record<string, boolean>>({});
  let confirm = $state<LlmLogEntry | null>(null);
  let confirmOpen = $state(false);
  let deleting = $state(false);
  let exporting = $state(false);

  const q = $derived(query.get('q') ?? '');

  async function load(search: string) {
    try {
      const r = await must(api.GET('/llm/log', { params: { query: { q: search || undefined, limit: 200 } } }));
      entries = r.entries;
      total = r.total;
      error = null;
      // A link to one entry (from a chat answer or advisor) opens it.
      if (r.entries.length === 1 || r.entries.some((e) => e.id === search)) {
        const hit = r.entries.find((e) => e.id === search) ?? r.entries[0];
        open = { ...open, [hit.id]: true };
      }
    } catch (e) {
      error = errorText(e);
    } finally {
      loaded = true;
    }
  }

  $effect.pre(() => {
    text = q;
  });
  $effect(() => {
    void load(q);
  });

  let timer: ReturnType<typeof setTimeout> | undefined;
  function oninput() {
    clearTimeout(timer);
    timer = setTimeout(go, 350);
  }
  function go() {
    clearTimeout(timer);
    const t = text.trim();
    if (t === q) return;
    router.replace(href('/llm-log', { q: t }).slice(1));
  }

  const shown = $derived(
    entries.filter((e) => (!surface || e.surface === surface) && (!rated || e.rating === rated)),
  );
  const counts = $derived(
    Object.fromEntries(SURFACES.map(([s]) => [s, entries.filter((e) => e.surface === s).length])) as Record<Surface, number>,
  );
  const ups = $derived(entries.filter((e) => e.rating === 'up').length);
  const downs = $derived(entries.filter((e) => e.rating === 'down').length);

  function toggle(id: string) {
    open = { ...open, [id]: !open[id] };
  }

  function firstLine(s: string): string {
    const l = s.replace(/[#*`>]/g, '').split('\n').find((x) => x.trim()) ?? '';
    return l.length > 160 ? l.slice(0, 157) + '…' : l;
  }

  /** The question the user asked: last "user:" line of a chat prompt. */
  function question(e: LlmLogEntry): string | undefined {
    if (e.surface !== 'chat') return undefined;
    const lines = e.prompt.split('\n').filter((l) => l.startsWith('user: '));
    return lines.length ? lines[lines.length - 1].slice(6) : undefined;
  }

  /** Prompt split around the redaction marker, for highlighting. */
  const parts = (s: string) => s.split(/(\[redacted\])/g);

  async function exportLog() {
    exporting = true;
    try {
      const body = await must(api.GET('/llm/log/export', { parseAs: 'text' }));
      const blob = new Blob([body as unknown as string], { type: 'application/x-ndjson' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `llm-log-${new Date().toISOString().slice(0, 10)}.jsonl`;
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      toasts.ok('LLM log exported.', `${total} entries as JSON lines.`);
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) toasts.error('Only platform admins can export the LLM log.', e);
      else toasts.error('Export failed', e);
    } finally {
      exporting = false;
    }
  }

  function askDelete(e: LlmLogEntry) {
    confirm = e;
    confirmOpen = true;
  }

  async function doDelete() {
    if (!confirm) return;
    const id = confirm.id;
    deleting = true;
    try {
      await must(api.DELETE('/llm/log/{id}', { params: { path: { id } } }));
      entries = entries.filter((e) => e.id !== id);
      total = Math.max(0, total - 1);
      confirmOpen = false;
      confirm = null;
      toasts.ok(`Deleted ${id}.`, 'The audit log records the deletion, not the content.');
    } catch (e) {
      toasts.error(`Could not delete ${id}`, e);
    } finally {
      deleting = false;
    }
  }
</script>

<div class="page">
  <header class="head">
    <div>
      <h1>LLM log</h1>
      <p class="muted small sub">
        Every model call: the prompt as sent (secrets already removed), the answer, its sources and your rating.
        Chat, advisors, commit messages and agents over MCP all land here.
      </p>
    </div>
    <span class="spacer"></span>
    <button class="btn small" onclick={exportLog} disabled={exporting} title="Download all entries as JSON lines (platform admins)">
      {#if exporting}<span class="spin"></span>{:else}<Glyph name="download" size={13} />{/if} Export
    </button>
  </header>

  <div class="tools">
    <div class="search">
      <Glyph name="search" size={15} />
      <input
        bind:value={text}
        {oninput}
        onkeydown={(e) => e.key === 'Enter' && go()}
        placeholder="Search prompts, answers, change ids, users…"
        aria-label="Search the LLM log"
        spellcheck="false"
      />
      {#if text}
        <button class="clr" onclick={() => ((text = ''), go())} aria-label="Clear search"><Glyph name="x" size={12} /></button>
      {/if}
    </div>
    <div class="filters" role="group" aria-label="Filter">
      <button class="chip-filter" aria-pressed={!surface} onclick={() => (surface = '')}>all {entries.length}</button>
      {#each SURFACES as [s, label] (s)}
        {#if counts[s]}
          <button class="chip-filter" aria-pressed={surface === s} onclick={() => (surface = surface === s ? '' : s)}>{label} {counts[s]}</button>
        {/if}
      {/each}
      <span class="sep"></span>
      <button class="chip-filter" aria-pressed={rated === 'up'} onclick={() => (rated = rated === 'up' ? '' : 'up')} title="Rated helpful">
        <Glyph name="thumbs-up" size={12} /> {ups}
      </button>
      <button class="chip-filter" aria-pressed={rated === 'down'} onclick={() => (rated = rated === 'down' ? '' : 'down')} title="Rated not helpful">
        <Glyph name="thumbs-down" size={12} /> {downs}
      </button>
    </div>
  </div>

  {#if error}
    <div class="error-box">{error}</div>
  {:else if !loaded}
    <p class="muted small">Loading…</p>
  {:else if !shown.length}
    <div class="empty">{q || surface || rated ? 'No entry matches.' : 'No model calls yet. Ask the chat or run an advisor.'}</div>
  {:else}
    <p class="small muted count">
      {shown.length === total ? `${total} ${total === 1 ? 'entry' : 'entries'}` : `${shown.length} of ${total} entries`}{q ? ` for “${q}”` : ''}
    </p>
    <ol class="log">
      {#each shown as e (e.id)}
        {@const isOpen = !!open[e.id]}
        {@const qq = question(e)}
        <li class="entry" class:open={isOpen} class:err={!!e.error}>
          <button class="row" onclick={() => toggle(e.id)} aria-expanded={isOpen}>
            <span class="when" title={dateTime(e.time)}>{ago(e.time)}</span>
            <span class="badge s-{e.surface}">{e.surface === 'commit-message' ? 'commit msg' : e.surface}{e.recipe && e.recipe !== e.surface ? ` · ${e.recipe}` : ''}</span>
            <span class="what">
              {#if qq}<span class="qq">{qq}</span><span class="muted arr">→</span>{/if}<span class="ans">{firstLine(e.answer)}</span>
            </span>
            <span class="meta">
              {#if e.redactions}<span class="red" title="Values the secret filter removed from the prompt"><Glyph name="lock" size={11} /> {e.redactions}</span>{/if}
              {#if e.error}<span class="bad-text" title={e.error}><Glyph name="alert" size={12} /></span>{/if}
              {#if e.rating}<span class="rt {e.rating}"><Glyph name={e.rating === 'up' ? 'thumbs-up' : 'thumbs-down'} size={12} /></span>{/if}
              <span class="who">{e.user}</span>
            </span>
            <Glyph name="chevron" size={12} />
          </button>
          {#if isOpen}
            <div class="detail">
              <dl class="facts">
                <div><dt>id</dt><dd class="mono">{e.id}</dd></div>
                <div><dt>time</dt><dd>{dateTime(e.time)}</dd></div>
                <div><dt>user</dt><dd>{e.user}</dd></div>
                <div><dt>model</dt><dd class="mono">{e.mock ? 'built-in (no model endpoint)' : e.model}</dd></div>
                {#if e.durationMs != null}<div><dt>took</dt><dd>{duration(e.durationMs)}</dd></div>{/if}
                {#if e.changeId}
                  <div>
                    <dt>change</dt>
                    <dd><a class="mono" href={href(isChangeId(e.changeId) ? `/changes/${e.changeId}` : '/history', isChangeId(e.changeId) ? undefined : { q: e.changeId })}>{e.changeId}</a></dd>
                  </div>
                {/if}
                <div><dt>redacted</dt><dd>{e.redactions ? `${e.redactions} value${e.redactions > 1 ? 's' : ''}` : 'none'}</dd></div>
              </dl>
              {#if e.error}
                <div class="callout warn"><Glyph name="alert" size={14} /> <span>The model call failed ({e.error}); the answer comes from the deterministic findings.</span></div>
              {/if}
              <div class="cols">
                <section>
                  <h3 class="eyebrow">Prompt as sent</h3>
                  <pre class="prompt">{#each parts(e.prompt) as p, i (i)}{#if p === '[redacted]'}<mark>{p}</mark>{:else}{p}{/if}{/each}</pre>
                </section>
                <section>
                  <h3 class="eyebrow">Answer</h3>
                  <div class="answer"><Markdown source={e.answer} /></div>
                  {#if e.sources?.length}
                    <h3 class="eyebrow">Sources</h3>
                    <div class="srcs">
                      {#each e.sources as s, i (i)}<SourceChip source={s} changeId={e.changeId} />{/each}
                    </div>
                  {/if}
                </section>
              </div>
              <div class="acts">
                <Thumbs logId={e.id} bind:rating={e.rating} note={false} />
                <span class="small muted">{e.rating ? `rated ${e.rating === 'up' ? 'helpful' : 'not helpful'}` : 'not rated'}</span>
                <span class="spacer"></span>
                <button class="btn small danger" onclick={() => askDelete(e)}><Glyph name="trash" size={13} /> Delete</button>
              </div>
            </div>
          {/if}
        </li>
      {/each}
    </ol>
  {/if}
</div>

<Modal bind:open={confirmOpen} title="Delete this LLM log entry?" icon="trash" width={480} onclose={() => (confirm = null)}>
  {#if confirm}
    <p>
      <span class="mono">{confirm.id}</span> ({confirm.surface}, {dateTime(confirm.time)}, {confirm.user}) goes away for everyone, with its
      prompt, answer and rating. This cannot be undone.
    </p>
    <p class="small muted">The audit log keeps a line that you deleted it, without the content.</p>
  {/if}
  {#snippet footer()}
    <button class="btn" onclick={() => (confirmOpen = false)}>Cancel</button>
    <button class="btn danger primary" onclick={doDelete} disabled={deleting}>
      {#if deleting}<span class="spin"></span>{/if} Delete entry
    </button>
  {/snippet}
</Modal>

<style>
  /* One column that may shrink below its content (phone width). */
  .page {
    grid-template-columns: minmax(0, 1fr);
  }
  .head {
    display: flex;
    align-items: flex-start;
    gap: 12px;
  }
  h1 {
    margin: 0;
    font-size: 22px;
  }
  .sub {
    margin: 4px 0 0;
    max-width: 680px;
  }
  .head .btn {
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
  .tools {
    display: grid;
    gap: 8px;
  }
  .search {
    display: flex;
    align-items: center;
    gap: 8px;
    border: 1px solid var(--line-strong);
    border-radius: 7px;
    padding: 6px 10px;
    color: var(--muted);
    background: var(--panel);
    max-width: 720px;
  }
  .search:focus-within {
    border-color: var(--accent-fill);
  }
  .search input {
    flex: 1;
    border: 0;
    outline: none;
    background: transparent;
    font-size: 14px;
    color: var(--ink);
    min-width: 0;
  }
  .clr {
    border: 0;
    background: none;
    color: var(--muted);
    display: grid;
    place-items: center;
  }
  .filters {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    align-items: center;
  }
  .filters .chip-filter {
    display: inline-flex;
    gap: 4px;
    align-items: center;
  }
  .sep {
    width: 1px;
    height: 18px;
    background: var(--line);
    margin: 0 4px;
  }
  .count {
    margin: 0;
  }
  .log {
    list-style: none;
    margin: 0;
    padding: 0;
    border: 1px solid var(--line);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .entry + .entry {
    border-top: 1px solid var(--line);
  }
  .entry.open {
    background: color-mix(in srgb, var(--accent-soft) 35%, var(--panel));
  }
  .row {
    display: flex;
    align-items: center;
    gap: 10px;
    width: 100%;
    border: 0;
    background: none;
    text-align: left;
    padding: 9px 12px;
    font-size: 13.5px;
    color: var(--ink);
    min-width: 0;
  }
  .row:hover {
    background: color-mix(in srgb, var(--accent-soft) 45%, transparent);
  }
  .row > :global(svg:last-child) {
    color: var(--muted);
    flex: none;
    transition: transform 0.15s;
  }
  .open .row > :global(svg:last-child) {
    transform: rotate(90deg);
  }
  .when {
    width: 64px;
    flex: none;
    color: var(--muted);
    font-size: 12px;
  }
  .badge {
    flex: none;
  }
  .s-chat {
    background: var(--accent-soft);
    color: var(--accent);
  }
  .s-advisor {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .s-mcp {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .what {
    flex: 1;
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .qq {
    font-weight: 600;
  }
  .arr {
    margin: 0 6px;
  }
  .meta {
    display: flex;
    gap: 8px;
    align-items: center;
    flex: none;
    font-size: 12px;
    color: var(--muted);
  }
  .red {
    display: inline-flex;
    gap: 3px;
    align-items: center;
    color: var(--warn);
  }
  .rt.up {
    color: var(--ok);
  }
  .rt.down {
    color: var(--bad);
  }
  .detail {
    padding: 4px 12px 14px 86px;
    display: grid;
    gap: 12px;
    min-width: 0;
  }
  .facts {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 22px;
    margin: 0;
    font-size: 13px;
  }
  .facts div {
    display: flex;
    gap: 6px;
  }
  .facts dt {
    color: var(--muted);
  }
  .facts dd {
    margin: 0;
  }
  .cols {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
    gap: 14px;
  }
  .cols section {
    display: grid;
    gap: 6px;
    align-content: start;
    min-width: 0;
  }
  .eyebrow {
    margin: 0;
  }
  .prompt {
    margin: 0;
    max-height: 320px;
    overflow: auto;
    white-space: pre-wrap;
    font-size: 12px;
    line-height: 1.5;
  }
  .prompt mark {
    background: var(--warn-soft);
    color: var(--warn);
    border-radius: 3px;
    padding: 0 2px;
  }
  .answer {
    font-size: 14px;
    border: 1px solid var(--line);
    border-radius: 7px;
    padding: 8px 12px;
    background: var(--panel);
  }
  .answer :global(.md p:last-child) {
    margin-bottom: 0;
  }
  .srcs {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .acts {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .acts .btn {
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
  @media (max-width: 860px) {
    .cols {
      grid-template-columns: minmax(0, 1fr);
    }
    .detail {
      padding-left: 12px;
    }
    .who {
      display: none;
    }
  }
</style>
