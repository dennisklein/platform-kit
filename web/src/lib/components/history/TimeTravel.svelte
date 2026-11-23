<script lang="ts">
  // Time travel: a file as it was at a merge commit (read-only), with its
  // stored schema version, converted to the newest version, and compared
  // with main now.
  import { api, must, errorText, ApiError } from '../../api/client';
  import type { FileContent, HistoryEntry, SemanticLine } from '../../api/ftypes';
  import { href } from '../../router.svelte';
  import { parseAllDocuments } from 'yaml';
  import { diffLines, hunks, stat } from '../../f/linediff';
  import { compareFields, type FieldChange } from '../../f/fielddiff';
  import { short } from '../../f/fmt';
  import YamlView from '../YamlView.svelte';
  import Glyph from '../Glyph.svelte';

  let {
    sha,
    files,
    semantic = [],
    later = [],
  }: { sha: string; files: string[]; semantic?: SemanticLine[]; later?: HistoryEntry[] } = $props();

  const candidates = $derived(files.filter((f) => /\.(ya?ml)$/.test(f) && !f.startsWith('.pk/changes/')));
  const preferred = $derived(
    semantic.map((l) => l.path).find((p) => p && candidates.includes(p)) ?? candidates[0] ?? files[0],
  );

  let path = $state('');
  let stored = $state<FileContent | null>(null);
  let converted = $state<FileContent | null>(null);
  let now = $state<FileContent | null>(null);
  let nowMissing = $state(false);
  let view = $state<'stored' | 'converted'>('stored');
  let comparing = $state(false);
  let error = $state<string | null>(null);
  let loading = $state(false);

  $effect.pre(() => {
    path = preferred ?? '';
    comparing = false;
  });

  async function read(ref: string, p: string, convert: boolean) {
    return must(api.GET('/refs/{ref}/file', { params: { path: { ref }, query: { path: p, convert } } }));
  }

  async function load(s: string, p: string) {
    stored = converted = now = null;
    nowMissing = false;
    error = null;
    if (!p) return;
    loading = true;
    try {
      stored = await read(s, p, false);
      const sv = stored.storedVersion;
      const nv = stored.kind?.apiVersion;
      // Old schema versions also get the converted view.
      converted = sv && nv && sv !== nv ? await read(s, p, true) : stored;
      view = 'stored';
    } catch (e) {
      error = errorText(e);
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void load(sha, path);
  });

  async function compare() {
    comparing = !comparing;
    if (!comparing || now || nowMissing) return;
    try {
      now = await read('main', path, true);
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) nowMissing = true;
      else error = errorText(e);
    }
  }

  const ver = (v?: string) => v?.split('/').pop() ?? '';
  const storedVer = $derived(stored?.storedVersion ?? stored?.kind?.apiVersion);
  const newest = $derived(stored?.kind?.apiVersion);
  const old = $derived(!!storedVer && !!newest && storedVer !== newest);
  const base = $derived(converted ?? stored);
  const d = $derived(comparing && base && now ? diffLines(base.content, now.content) : []);
  const h = $derived(hunks(d, 2));
  const st = $derived(stat(d));

  // Field-level compare (format and key order do not matter); null when a
  // side does not parse as YAML.
  function docs(text: string): unknown[] | null {
    try {
      const ds = parseAllDocuments(text);
      if (!Array.isArray(ds) || ds.some((x) => x.errors.length)) return null;
      return ds.map((x) => x.toJS());
    } catch {
      return null;
    }
  }
  const fields = $derived.by((): FieldChange[] | null => {
    if (!comparing || !base || !now) return null;
    const a = docs(base.content);
    const b = docs(now.content);
    if (!a || !b) return null;
    return compareFields(a.length === 1 ? a[0] : a, b.length === 1 ? b[0] : b);
  });
  let asText = $state(false);

  // Lines the semantic diff names in this file (as stored at the merge).
  const marks = $derived(semantic.filter((l) => l.path === path && l.line).map((l) => l.line as number));
  const showMarks = $derived(view === 'stored' || !old);
  let codeEl: HTMLDivElement | undefined = $state();
  $effect(() => {
    if (!codeEl || !stored || !showMarks || !marks.length) return;
    const el = codeEl.querySelector<HTMLElement>('.hl');
    if (el) codeEl.scrollTop = Math.max(0, el.offsetTop - codeEl.clientHeight / 3);
  });
  const since = $derived(later.filter((e) => e.files?.includes(path)));
</script>

<section class="tt" aria-label="Time travel">
  <header>
    <Glyph name="commit" size={16} />
    <strong>State at {short(sha)} · read-only</strong>
    <span class="spacer"></span>
    <button class="btn small cmp" class:on={comparing} onclick={compare} disabled={!stored}>
      {comparing ? 'Hide compare' : 'Compare with now'}
    </button>
  </header>

  <div class="bar">
    {#if candidates.length > 1}
      <select class="select" bind:value={path} aria-label="File">
        {#each candidates as f (f)}<option value={f}>{f}</option>{/each}
      </select>
    {:else}
      <span class="mono p">{path}</span>
    {/if}
    {#if storedVer}
      <span class="sv" class:old title="apiVersion as stored in git at {short(sha)}">stored as {ver(storedVer)}</span>
    {/if}
    {#if old && converted && converted !== stored}
      <div class="seg" role="group" aria-label="View">
        <button aria-pressed={view === 'stored'} onclick={() => (view = 'stored')}>as stored</button>
        <button aria-pressed={view === 'converted'} onclick={() => (view = 'converted')}>converted to {ver(newest)}</button>
      </div>
    {/if}
  </div>

  {#if error}
    <div class="err">{error}</div>
  {:else if loading && !stored}
    <p class="muted small">Loading…</p>
  {:else if comparing}
    {#if nowMissing}
      <p class="note">{path} does not exist on main any more.</p>
    {:else if now && fields && !asText}
      {#if !fields.length}
        <p class="note"><Glyph name="check" size={13} /> No field changed since {short(sha)}{old ? ` (compared as ${ver(newest)})` : ''}.</p>
      {:else}
        <p class="note">{fields.length} field{fields.length > 1 ? 's' : ''} changed since {short(sha)}{old ? `, both as ${ver(newest)}` : ''}:</p>
        <table class="fd">
          <thead><tr><th>Field</th><th>at {short(sha)}</th><th>now</th></tr></thead>
          <tbody>
            {#each fields as f (f.field)}
              <tr class="f-{f.op}">
                <td class="mono">{f.field}</td>
                <td class="mono">{f.before ?? '—'}</td>
                <td class="mono">{f.after ?? '—'}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
      {#if since.length}
        <p class="note small">
          Changed since by
          {#each since as e, i (e.sha)}{i ? ', ' : ' '}<a class="lk mono" href={href(`/history/${e.sha}`)} title={e.title}>{e.change ?? e.short}</a>{/each}.
        </p>
      {/if}
      <div class="row2">
        <button class="linkbtn small" onclick={() => (asText = true)}>Show text diff</button>
        <a class="small open" href={href('/files', { path })}>Open on main <Glyph name="arrow-right" size={12} /></a>
      </div>
    {:else if now}
      {#if fields}<button class="linkbtn small" onclick={() => (asText = false)}>Show fields</button>{/if}
      {#if !d.length || (st.added === 0 && st.removed === 0)}
        <p class="note"><Glyph name="check" size={13} /> Unchanged since {short(sha)}{old ? ` (compared as ${ver(newest)})` : ''}.</p>
      {:else}
        <p class="note">
          {short(sha)} → main now{old ? `, both as ${ver(newest)}` : ''}: <span class="add">+{st.added}</span> <span class="del">−{st.removed}</span>
        </p>
        <pre class="cmp-diff">{#each h as hk, i (i)}<span class="hh">{hk.header}</span>{#each hk.lines as l, j (j)}<span class="dl op{l.op === '+' ? 'a' : l.op === '-' ? 'd' : 'c'}"><span class="m">{l.op}</span>{l.text}</span>{/each}{/each}</pre>
      {/if}
      <a class="small open" href={href('/files', { path })}>Open on main <Glyph name="arrow-right" size={12} /></a>
    {:else}
      <p class="muted small">Loading main…</p>
    {/if}
  {:else if stored}
    <div class="code" bind:this={codeEl}>
      <YamlView text={view === 'converted' && converted ? converted.content : stored.content} numbers highlight={showMarks ? marks : []} />
    </div>
  {/if}
</section>

<style>
  .tt {
    background: var(--gsi-navy);
    color: #e7e9ee;
    border-radius: var(--radius);
    padding: 12px 14px;
    display: grid;
    gap: 10px;
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-fill) 55%, transparent);
    min-width: 0;
  }
  header {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  header :global(svg) {
    color: var(--gsi-orange);
  }
  .cmp {
    background: transparent;
    color: #fff;
    border-color: #6b7280;
  }
  .cmp:hover:not(:disabled) {
    background: rgb(255 255 255 / 0.08) !important;
  }
  .cmp.on {
    border-color: var(--gsi-orange);
    color: var(--gsi-orange);
  }
  .bar {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    font-size: 13px;
  }
  .bar .select {
    background: #1b1e24;
    color: #e7e9ee;
    border-color: #4b5160;
    max-width: 100%;
  }
  .p {
    color: #c9ccd2;
  }
  .sv {
    font: 500 12px/1.7 var(--mono);
    background: #eef0f3;
    color: #20242b;
    padding: 0 8px;
    border-radius: 4px;
  }
  .sv.old {
    background: var(--gsi-orange);
    color: #131519;
  }
  .seg {
    display: inline-flex;
    border: 1px solid #4b5160;
    border-radius: 6px;
    overflow: hidden;
  }
  .seg button {
    border: 0;
    background: transparent;
    color: #c9ccd2;
    font-size: 12px;
    padding: 2px 9px;
  }
  .seg button[aria-pressed='true'] {
    background: #3b414c;
    color: #fff;
  }
  .code {
    position: relative;
    max-height: 340px;
    overflow: auto;
    background: #15181d;
    border-radius: 6px;
  }
  .code :global(pre) {
    background: transparent;
    border: 0;
    color: #e7e9ee;
  }
  .code :global(.y-k) {
    color: #7fb6e6;
  }
  .code :global(.y-v) {
    color: #a5d6a7;
  }
  .code :global(.y-n) {
    color: #f2c14e;
  }
  /* Long flow-style lines wrap under their text, not under the number. */
  .code :global(pre.numbers) {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .code :global(pre.numbers .ln) {
    padding-left: 38px;
    text-indent: -38px;
  }
  .code :global(pre.numbers .no) {
    text-indent: 0;
  }
  .code :global(.ln.hl) {
    background: rgb(245 166 35 / 0.18);
    box-shadow: inset 3px 0 0 var(--gsi-orange);
  }
  .note {
    margin: 0;
    font-size: 13px;
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .add {
    color: #7fd36a;
    font-family: var(--mono);
  }
  .del {
    color: #ff8a80;
    font-family: var(--mono);
  }
  .cmp-diff {
    background: #15181d;
    border: 0;
    color: #e7e9ee;
    font-size: 12px;
    max-height: 340px;
    overflow: auto;
    padding: 6px 0;
  }
  .hh {
    display: block;
    color: #8a96a6;
    padding: 2px 12px;
  }
  .dl {
    display: block;
    white-space: pre;
    padding-right: 12px;
  }
  .m {
    display: inline-block;
    width: 22px;
    text-align: center;
    color: #8a96a6;
  }
  .opa {
    background: rgb(95 207 150 / 0.16);
  }
  .opd {
    background: rgb(255 138 128 / 0.16);
  }
  .fd {
    width: 100%;
    border-collapse: collapse;
    font-size: 12.5px;
  }
  .fd th {
    text-align: left;
    font-weight: 600;
    color: #8a96a6;
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    padding: 4px 8px;
    border-bottom: 1px solid #3b414c;
  }
  .fd td {
    padding: 4px 8px;
    border-bottom: 1px solid #2e333c;
    word-break: break-all;
  }
  .f-add td:last-child {
    color: #7fd36a;
  }
  .f-delete td:nth-child(2) {
    color: #ff8a80;
  }
  .f-change td:last-child {
    color: var(--gsi-orange);
  }
  .lk {
    color: var(--gsi-orange);
  }
  .row2 {
    display: flex;
    gap: 14px;
    align-items: center;
  }
  .linkbtn {
    border: 0;
    background: none;
    padding: 0;
    color: #c9ccd2;
    text-decoration: underline;
    justify-self: start;
  }
  .err {
    color: #ff8a80;
    font-size: 13px;
  }
  .open {
    color: var(--gsi-orange);
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
</style>
