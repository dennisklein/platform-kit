<script lang="ts">
  // One entity: Intent and Hardware facts (truth class and owner per field),
  // ports and cables with checks, semantic diff, references and backlinks.
  import type { Entity, EntityField } from '../../api/ftypes';
  import { href } from '../../router.svelte';
  import { isChangeId } from '../../f/fmt';
  import TruthBadge from '../TruthBadge.svelte';
  import SemanticDiff from '../SemanticDiff.svelte';
  import ChangeChip from '../ChangeChip.svelte';
  import Glyph from '../Glyph.svelte';
  import JbodSlots from './JbodSlots.svelte';

  let { entity }: { entity: Entity } = $props();

  const ref = $derived(entity.ref && entity.ref !== 'main' ? entity.ref : undefined);
  const fileRoute = (path: string, line?: number) =>
    isChangeId(ref)
      ? href(`/changes/${ref}/files`, { path, line: line ? String(line) : undefined })
      : href('/files', { path, line: line ? String(line) : undefined });

  const version = $derived(entity.apiVersion.split('/').pop());
  const isNode = $derived(entity.kind === 'Node');
  const icon = $derived(
    ({ Node: 'server', Rack: 'board', Switch: 'link', Jbod: 'disk', Cluster: 'hex', CablePlan: 'link' } as Record<string, string>)[
      entity.kind
    ] ?? 'file',
  );

  // Fields by UI group; ports have their own table.
  const groups = $derived.by(() => {
    const out = new Map<string, EntityField[]>();
    // Labels are intent, but rarely the point: last, and not at all when empty.
    const fields = [...entity.fields].sort((a, b) => +isLabels(a) - +isLabels(b));
    for (const f of fields) {
      if (isLabels(f) && isEmpty(f.value) && !f.display) continue;
      if (f.field === 'spec.ports' && entity.ports?.length) continue;
      if (f.field === 'spec.drawers') continue;
      const g = f.group ?? (f.owner || f.truth === 'asserted' ? 'Hardware facts' : f.truth === 'derived' ? 'Derived' : 'Intent');
      if (g === 'Ports and cables') continue;
      if (!out.has(g)) out.set(g, []);
      out.get(g)!.push(f);
    }
    return [...out.entries()];
  });

  function isLabels(f: EntityField) {
    return f.field === 'metadata.labels' || f.field === 'labels';
  }
  function isEmpty(v: unknown) {
    return v === undefined || v === null || v === '' || (typeof v === 'object' && Object.keys(v as object).length === 0);
  }

  const drawers = $derived(entity.fields.find((f) => f.field === 'spec.drawers')?.value as
    | { id: number; slots: { slot: number; state: string; serial?: string }[] }[]
    | undefined);

  function show(f: EntityField): string {
    if (f.secret) return '••••••••';
    if (f.display) return f.display;
    const v = f.value;
    if (v === undefined || v === null || v === '') return '—';
    if (typeof v === 'object') {
      if (Array.isArray(v)) return `${v.length} items`;
      return Object.entries(v as Record<string, unknown>)
        .map(([k, x]) => `${k}=${typeof x === 'object' ? JSON.stringify(x) : x}`)
        .join(', ');
    }
    return String(v);
  }

  // Values that are entity names link to the entity.
  const refByField = $derived(new Map(entity.references.map((r) => [r.field, r])));
  const entityRoute = (kind: string, name: string) =>
    kind === 'Rack' ? href(`/assets/racks/${encodeURIComponent(name)}`, { ref }) : href(`/assets/${kind}/${encodeURIComponent(name)}`, { ref });

  const bl = $derived(entity.backlinks);
  const isNew = $derived(entity.change?.status === 'added');
  const badPorts = $derived((entity.ports ?? []).filter((p) => !p.ok).length);
</script>

<div class="ev">
  <header class="head">
    <span class="ico"><Glyph name={icon} size={24} /></span>
    <h1 class="mono">{entity.name}</h1>
    <span class="kv">{entity.kind} {version}</span>
    {#if entity.storedVersion && entity.storedVersion !== entity.apiVersion}
      <span class="badge" title="stored as {entity.storedVersion}">stored {entity.storedVersion.split('/').pop()}</span>
    {/if}
    {#if entity.change?.id}
      <a class="new st-{entity.change.status}" href={href(`/changes/${entity.change.id}`)}>
        {isNew ? 'new' : entity.change.status === 'deleted' ? 'deleted' : 'changed'} in {entity.change.id}
      </a>
    {/if}
    <span class="spacer"></span>
    <a class="path" href={fileRoute(entity.path)} title="Open the file{ref ? ` in ${ref}` : ' (main, read-only)'}">{entity.path}</a>
  </header>
  {#if entity.title && !/node node$/.test(entity.title)}<p class="muted sub">{entity.title}</p>{/if}

  {#if entity.problems.length}
    <ul class="problems">
      {#each entity.problems as p, i (i)}
        <li class="sev-{p.severity}">
          <Glyph name="alert" size={14} />
          <span>{p.message}</span>
          <a class="mono small" href={fileRoute(p.path, p.line)}>{p.path.split('/').pop()}{p.line ? `:${p.line}` : ''}</a>
        </li>
      {/each}
    </ul>
  {/if}

  <div class="cards">
    {#each groups as [g, fields] (g)}
      <section class="card">
        <h3>{g}</h3>
        <table class="ftable">
          <tbody>
            {#each fields as f (f.field)}
              {@const r = refByField.get(f.field)}
              <tr>
                <th scope="row" title={f.field}>{f.label}</th>
                <td class="val mono">
                  {#if r && r.exists}
                    <a href={entityRoute(r.kind, r.name)}>{show(f)}</a>
                  {:else if f.source?.path}
                    <a class="plain" href={fileRoute(f.source.path, f.source.line)} title="{f.source.path}:{f.source.line}">{show(f)}</a>
                  {:else}
                    {show(f)}
                  {/if}
                </td>
                <td class="tc"><TruthBadge truth={f.truth} owner={f.owner} /></td>
              </tr>
            {/each}
            {#if g === 'Intent' && entity.applyPath}
              <tr>
                <th scope="row">Apply path</th>
                <td class="val">{entity.applyPath}</td>
                <td class="tc"><TruthBadge truth="derived" /></td>
              </tr>
            {/if}
          </tbody>
        </table>
      </section>
    {/each}
  </div>

  {#if entity.ports?.length}
    <section class="card">
      <h3>
        Ports and cables
        {#if badPorts}<span class="bad-text cnt">{badPorts} problem{badPorts > 1 ? 's' : ''}</span>{/if}
      </h3>
      <table class="table ports">
        <thead><tr><th>Port</th><th>Cable</th><th>Peer</th><th>Network</th><th>Check</th></tr></thead>
        <tbody>
          {#each entity.ports as p (p.port)}
            <tr class:bad={!p.ok}>
              <td class="mono">{p.port}</td>
              <td class="mono">{p.cable ?? '—'}</td>
              <td class="mono">
                {#if p.peer}
                  {@const sw = p.peer.split(' ')[0]}
                  <a class="plain" href={entityRoute('Switch', sw)}>{p.peer}</a>
                {:else}—{/if}
              </td>
              <td class="mono">{p.network ?? '—'}</td>
              <td class="chk">
                {#if p.ok}
                  <span class="ok-text"><Glyph name="check" size={15} title="ok" /></span>
                {:else}
                  <span class="bad-text"><Glyph name="x" size={15} title="problem" /> {p.message ?? 'problem'}</span>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </section>
  {/if}

  {#if drawers?.length}
    <section class="card">
      <h3>Drawers and slots</h3>
      <JbodSlots {drawers} />
    </section>
  {/if}

  {#if entity.semantic?.length}
    <section class="card sem">
      <h3>Semantic diff{ref ? ` · ${ref}` : ''}</h3>
      <SemanticDiff lines={entity.semantic} fileHref={(l) => (l.path ? fileRoute(l.path, l.line) : undefined)} />
    </section>
  {/if}

  <div class="cards links">
    <section class="card">
      <h3>References</h3>
      {#each entity.references as r (r.field + r.name)}
        <div class="lnk">
          {#if r.exists}
            <a class="mono" href={entityRoute(r.kind, r.name)}>{r.name}</a>
          {:else}
            <span class="mono bad-text" title="not found">{r.name}</span>
          {/if}
          <span class="muted small">{r.kind}</span>
          <span class="spacer"></span>
          <span class="mono small muted" title="field">{r.field}</span>
          <span class="chip {r.exists ? 'ok' : 'bad'}">{r.exists ? 'ok' : 'missing'}</span>
        </div>
      {:else}
        <p class="muted small">No references.</p>
      {/each}
    </section>

    <section class="card">
      <h3>Backlinks</h3>
      {#if bl.changes.length}
        <h4>Changes</h4>
        {#each bl.changes as c (c.id + (c.sha ?? ''))}
          <div class="lnk"><ChangeChip change={c} title /></div>
        {/each}
        <a class="small more" href={href('/history', { entity: entity.name })}>History of {entity.name} <Glyph name="arrow-right" size={12} /></a>
      {/if}
      {#if bl.articles.length}
        <h4>Articles</h4>
        {#each bl.articles as a (a.path)}
          <div class="lnk">
            <Glyph name="book" size={14} />
            <a href={href('/kb/article', { path: a.path })}>{a.title}</a>
            <span class="spacer"></span>
            <span class="badge">{a.type}</span>
          </div>
        {/each}
      {/if}
      {#if bl.workItems.length}
        <h4>Work items</h4>
        {#each bl.workItems as w (w.id)}
          <div class="lnk">
            <a class="mono" href={href('/work', { item: w.id })}>{w.id}</a>
            <span class="t">{w.title}</span>
            <span class="spacer"></span>
            <span class="badge">{w.status}</span>
          </div>
        {/each}
      {/if}
      {#if bl.entities.length}
        <h4>Used by</h4>
        <div class="used">
          {#each bl.entities as r (r.kind + r.name + r.field)}
            <a class="mono small" href={entityRoute(r.kind, r.name)} title="{r.kind} · {r.field}">{r.name}</a>
          {/each}
        </div>
      {/if}
      {#if !bl.changes.length && !bl.articles.length && !bl.workItems.length && !bl.entities.length}
        <p class="muted small">Nothing links here yet.</p>
      {/if}
    </section>
  </div>
</div>

<style>
  .ev {
    display: grid;
    gap: 14px;
    min-width: 0;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .ico {
    color: var(--info);
    display: inline-flex;
  }
  h1 {
    font-size: 24px;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .kv {
    font: 600 12px/1 var(--mono);
    background: var(--gsi-navy);
    color: #fff;
    padding: 5px 8px;
    border-radius: 4px;
  }
  .new {
    font: 600 12px/1 var(--sans);
    background: var(--ok-soft);
    color: var(--ok);
    padding: 5px 8px;
    border-radius: 4px;
    text-decoration: none;
  }
  .new.st-modified {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .new.st-deleted {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .path {
    color: var(--muted);
    font-size: 13px;
    text-decoration: none;
  }
  .path:hover {
    color: var(--accent);
  }
  .sub {
    margin: -8px 0 0;
  }
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(340px, 100%), 1fr));
    gap: 14px;
  }
  .card {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 12px 16px;
    min-width: 0;
  }
  .card h3 {
    margin-bottom: 8px;
    display: flex;
    gap: 8px;
    align-items: baseline;
  }
  .cnt {
    text-transform: none;
    letter-spacing: 0;
    font-size: 12px;
  }
  .card h4 {
    font-size: 12px;
    color: var(--muted);
    font-weight: 600;
    margin: 10px 0 4px;
  }
  .card h4:first-of-type {
    margin-top: 0;
  }
  .ftable {
    width: 100%;
    border-collapse: collapse;
    font-size: 13.5px;
  }
  .ftable th,
  .ftable td {
    padding: 6px 0;
    border-bottom: 1px solid var(--line);
    vertical-align: baseline;
  }
  .ftable tr:last-child th,
  .ftable tr:last-child td {
    border-bottom: 0;
  }
  .ftable th {
    text-align: left;
    font-weight: 400;
    color: var(--muted);
    width: 34%;
    padding-right: 10px;
  }
  .val {
    font-size: 13px;
    overflow-wrap: anywhere;
  }
  .val a {
    color: var(--ink);
  }
  a.plain {
    color: inherit;
    text-decoration: none;
  }
  a.plain:hover {
    text-decoration: underline;
  }
  .tc {
    text-align: right;
    width: 1%;
    padding-left: 12px;
    white-space: nowrap;
  }
  .ports td {
    vertical-align: middle;
  }
  .ports tr.bad td {
    background: var(--bad-soft);
  }
  .chk span {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font: 500 12.5px var(--mono);
  }
  .sem {
    border: 2px solid color-mix(in srgb, var(--accent-fill) 60%, transparent);
    background: color-mix(in srgb, var(--accent-soft) 35%, var(--panel));
  }
  .problems {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .problems li {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px;
    border-radius: var(--radius-s);
    background: var(--bad-soft);
    color: var(--bad);
    font-size: 13px;
  }
  .problems li.sev-warning {
    background: var(--warn-soft);
    color: var(--warn);
  }
  .problems li.sev-info {
    background: var(--info-soft);
    color: var(--info);
  }
  .problems a {
    margin-left: auto;
    color: inherit;
  }
  .lnk {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 0;
    font-size: 13px;
    min-width: 0;
  }
  .lnk :global(.cc) {
    flex: 1;
  }
  .lnk .t {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    min-width: 0;
  }
  .chip {
    font-size: 11px;
    font-weight: 600;
    padding: 0 7px;
    border-radius: 4px;
  }
  .chip.ok {
    background: var(--ok-soft);
    color: var(--ok);
  }
  .chip.bad {
    background: var(--bad-soft);
    color: var(--bad);
  }
  .used {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 10px;
  }
  .more {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    margin-top: 4px;
  }
  /* Wide tables scroll on their own at phone width. */
  @media (max-width: 700px) {
    .table {
      display: block;
      overflow-x: auto;
    }
    .table :is(td, th) {
      white-space: nowrap;
    }
  }
</style>
