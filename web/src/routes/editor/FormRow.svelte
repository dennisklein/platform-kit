<script lang="ts">
  // One form row: label (with path, truth class, owner) and a widget.
  // Lists render their items inline, with the item's fields side by side.
  import FormRow from './FormRow.svelte';
  import type { Problem } from '../../lib/api/types';
  import { rowsOf, problemsAt, nextPortName, MASKED, type Row } from '../../lib/change/formModel';
  import { checkValue, at, skeleton, type JS } from '../../lib/change/schema';
  import { formatPath, getAt, type Path } from '../../lib/change/yamlEdit';
  import { entities, unitInfo } from '../../lib/change/entities.svelte';
  import Icon from '../../lib/components/Icon.svelte';
  import { href } from '../../lib/router.svelte';

  let {
    row,
    root,
    data,
    problems,
    readOnly,
    changeId,
    secrets = [],
    inline = false,
    onset,
    onappend,
    ondelete,
  }: {
    row: Row;
    root: JS;
    data: unknown;
    problems: Problem[];
    readOnly: boolean;
    changeId?: string;
    secrets?: { field: string; kind: string; key?: string }[];
    inline?: boolean;
    onset: (path: Path, value: unknown) => void;
    onappend: (path: Path, value: unknown) => void;
    ondelete: (path: Path) => void;
  } = $props();

  const field = $derived(formatPath(row.path));
  const mine = $derived(problemsAt(problems, field));
  const local = $derived(row.kind === 'text' || row.kind === 'duration' || row.kind === 'number' ? checkValue(row.schema, row.value) : null);
  const errText = $derived(mine.length ? mine.map((p) => p.message.replace(`${field}: `, '')).join('; ') : local);
  const bad = $derived(mine.some((p) => p.severity === 'error') || !!local);
  const ro = $derived(readOnly || row.readOnly);
  const self = $derived(String(getAt(data, ['metadata', 'name']) ?? ''));

  function str(v: unknown): string {
    return v === undefined || v === null ? '' : String(v);
  }
  function setNum(path: Path, raw: string) {
    if (raw === '') return;
    const n = Number(raw);
    if (Number.isFinite(n)) onset(path, n);
  }

  const options = $derived.by(() => {
    if (row.kind !== 'select') return [];
    let o: string[] = (row.options ?? []).map(String);
    if (!o.length && row.refKind) o = entities.list(row.refKind, changeId).map((e) => e.name);
    const v = str(row.value);
    if (v && !o.includes(v)) o = [v, ...o];
    return o;
  });

  // rack picker: {rack, unit, size}
  const loc = $derived((row.value ?? {}) as { rack?: string; unit?: number; size?: number });
  const racks = $derived(row.kind === 'rack' ? entities.list('Rack', changeId).map((e) => e.name) : []);
  const rackInfo = $derived(
    row.kind === 'rack' && loc.rack ? unitInfo(entities.rack(loc.rack, changeId), Number(loc.unit ?? 1), Number(loc.size ?? 2), self) : null,
  );

  // port picker: {switch, port}
  const peer = $derived((row.value ?? {}) as { switch?: string; port?: number });
  const switches = $derived(row.kind === 'port' ? entities.list('Switch', changeId).map((e) => e.name) : []);

  // A port problem shows under the picker, with the node that holds the
  // port as a link: "port data-a 37 in use by cn-003".
  const portProblem = $derived(row.kind === 'port' ? mine.find((p) => p.field?.startsWith(field)) : undefined);
  const portHolder = $derived(portProblem?.message.match(/^(.*in use by )([A-Za-z0-9][\w.-]*)(.*)$/));

  const secret = $derived(secrets.find((s) => s.field === field));

  // Derived fields (for example the emulation fixture of a node) are not in
  // the file: show what the bench computes for this object.
  const docKind = $derived(String(getAt(data, ['kind']) ?? ''));
  const derivedText = $derived(
    row.kind === 'text' && (row.truth === 'derived' || row.truth === 'generated') && !str(row.value) && self
      ? (entities.detail(docKind, self, changeId)?.fields.find((f) => f.field === field)?.display ?? '')
      : '',
  );
  // Facts owned by another system (model): offer the values other objects
  // of this kind share.
  const suggest = $derived(row.kind === 'text' && row.owner && !ro && docKind ? entities.values(docKind, field) : []);
  const listId = $derived(`fr-${field.replace(/[^\w-]/g, '-')}`);

  // lists
  const items = $derived(Array.isArray(row.value) ? (row.value as unknown[]) : []);
  const itemSchema = $derived(row.kind === 'list' ? (at(root, [...row.path, 0]) ?? {}) : {});
  function addItem() {
    const v = skeleton(root, itemSchema) as Record<string, unknown>;
    if (v && typeof v === 'object' && 'name' in v && 'peer' in v) {
      // A node port, in the key order of the inventory files. The port
      // number starts at the previous port's (nodes use the same row).
      const name = nextPortName(items);
      const prev = items.length ? (items[items.length - 1] as { peer?: { port?: number } })?.peer?.port : undefined;
      const { name: _n, cable: _c, peer: _p, network: _w, ...rest } = v;
      onappend(row.path, {
        name,
        cable: '',
        peer: { switch: name === 'bmc' ? 'mgmt' : '', port: typeof prev === 'number' ? prev : 1 },
        network: name === 'bmc' ? 'mgmt' : 'data',
        ...rest,
      });
      return;
    }
    onappend(row.path, v);
  }

  let tagText = $state('');
  $effect(() => {
    if (row.kind === 'tags') tagText = (Array.isArray(row.value) ? row.value : []).join(', ');
  });

  let mapKey = $state('');
</script>

{#snippet portErr()}
  {#if portProblem}
    <span class="err porterr" class:warnt={portProblem.severity !== 'error'}>
      {#if portHolder}{portHolder[1]}<a href={href(`/assets/Node/${encodeURIComponent(portHolder[2])}`, changeId ? { ref: changeId } : {})}>{portHolder[2]}</a>{portHolder[3]}{:else}{portProblem.message}{/if}
    </span>
  {/if}
{/snippet}

{#snippet control()}
  {#if row.kind === 'text' && ro && derivedText}
    <input class="input mono" value={derivedText} readonly aria-label={row.label} title="Derived by the bench, not stored in the file" />
  {:else if row.kind === 'text'}
    <input
      class="input mono"
      class:bad
      value={str(row.value)}
      readonly={ro}
      placeholder={ro ? 'derived' : undefined}
      list={suggest.length ? listId : undefined}
      oninput={(e) => onset(row.path, e.currentTarget.value)}
      aria-label={row.label}
    />
    {#if suggest.length}
      <datalist id={listId}>{#each suggest as v (v)}<option value={v}></option>{/each}</datalist>
    {/if}
  {:else if row.kind === 'number'}
    <span class="with-unit">
      <input class="input mono num" class:bad type="number" value={str(row.value)} readonly={ro} min={row.schema.minimum} max={row.schema.maximum} oninput={(e) => setNum(row.path, e.currentTarget.value)} aria-label={row.label} />
      {#if row.unit}<span class="unit">{row.unit}</span>{/if}
    </span>
  {:else if row.kind === 'select'}
    <span class="hinted">
      <select class="input mono" class:bad value={str(row.value)} disabled={ro} onchange={(e) => onset(row.path, e.currentTarget.value)} aria-label={row.label}>
        {#if !str(row.value)}<option value="">—</option>{/if}
        {#each options as o (o)}<option value={o}>{o}</option>{/each}
      </select>
    </span>
  {:else if row.kind === 'boolean'}
    <label class="toggle">
      <input type="checkbox" checked={row.value === true} disabled={ro} onchange={(e) => onset(row.path, e.currentTarget.checked)} />
      <span class="track" aria-hidden="true"></span>
      <span class="sr-only">{row.label}</span>
    </label>
  {:else if row.kind === 'duration'}
    <span class="hinted">
      <input class="input mono" class:bad value={str(row.value)} readonly={ro} oninput={(e) => onset(row.path, e.currentTarget.value)} aria-label={row.label} />
      <span class="tag">duration</span>
    </span>
  {:else if row.kind === 'secret'}
    <span class="hinted secret" title="Secret value: the bench never sends it to the browser or the LLM. It keeps its stored value on save.">
      <Icon name="lock" size={14} />
      <span class="mono dots">{MASKED}</span>
      <span class="tag">{secret?.kind === 'openbao' ? 'OpenBao' : 'SOPS'}</span>
    </span>
  {:else if row.kind === 'rack'}
    <span class="picker" class:bad={rackInfo && !rackInfo.ok}>
      <select class="bare mono" value={loc.rack ?? ''} disabled={ro} onchange={(e) => onset([...row.path, 'rack'], e.currentTarget.value)} aria-label="Rack">
        {#if !loc.rack}<option value="">rack…</option>{/if}
        {#each loc.rack && !racks.includes(loc.rack) ? [loc.rack, ...racks] : racks as r (r)}<option value={r}>{r}</option>{/each}
      </select>
      <span class="sep">·</span>
      <span class="mono">U</span>
      <input class="bare mono num2" type="number" min="1" max="47" value={str(loc.unit)} readonly={ro} oninput={(e) => setNum([...row.path, 'unit'], e.currentTarget.value)} aria-label="Unit" />
      <span class="muted small">{loc.size ?? 1}U</span>
      <span class="spacer"></span>
      <span class="tag fit" title={rackInfo?.text} class:ok={rackInfo?.ok && !!loc.rack} class:badt={rackInfo && !rackInfo.ok}>{rackInfo?.text ?? 'rack picker'}</span>
    </span>
  {:else if row.kind === 'port'}
    <span class="picker" class:bad={!!portProblem}>
      <select class="bare mono" value={peer.switch ?? ''} disabled={ro} onchange={(e) => onset([...row.path, 'switch'], e.currentTarget.value)} aria-label="Switch">
        {#if !peer.switch}<option value="">switch…</option>{/if}
        {#each peer.switch && !switches.includes(peer.switch) ? [peer.switch, ...switches] : switches as s (s)}<option value={s}>{s}</option>{/each}
      </select>
      <input class="bare mono num2" type="number" min="1" value={str(peer.port)} readonly={ro} oninput={(e) => setNum([...row.path, 'port'], e.currentTarget.value)} aria-label="Switch port" />
      <span class="spacer"></span>
      {#if portProblem}
        <span class="tag badt icon" title={portProblem.message}><Icon name="x" size={14} /></span>
      {:else if peer.switch && peer.port}
        <span class="tag ok">port free</span>
      {:else}
        <span class="tag">port picker</span>
      {/if}
    </span>
  {:else if row.kind === 'tags'}
    <input
      class="input mono"
      bind:value={tagText}
      readonly={ro}
      onchange={() => onset(row.path, tagText.split(/[\s,]+/).filter(Boolean))}
      aria-label={row.label}
      placeholder="comma separated"
    />
  {:else if row.kind === 'map'}
    {@const entries = Object.entries((row.value ?? {}) as Record<string, unknown>)}
    <div class="map">
      {#each entries as [k, v] (k)}
        <span class="mono k">{k}</span>
        <input class="input mono" value={str(v)} readonly={ro} oninput={(e) => onset([...row.path, k], e.currentTarget.value)} aria-label={k} />
        {#if !ro}<button class="btn ghost small" title="Remove {k}" onclick={() => ondelete([...row.path, k])}><Icon name="close" size={12} /></button>{:else}<span></span>{/if}
      {/each}
      {#if !ro}
        <input class="input mono" placeholder="new key" bind:value={mapKey} aria-label="New key" />
        <span></span>
        <button class="btn small" disabled={!mapKey.trim()} onclick={() => { onset([...row.path, mapKey.trim()], ''); mapKey = ''; }}>Add</button>
      {/if}
    </div>
  {:else if row.kind === 'yaml'}
    <span class="muted small">Edit this field in YAML.</span>
  {/if}
{/snippet}

{#if row.kind === 'list'}
  <div class="list">
    {#each items as it, i (i)}
      {@const sub = rowsOf(root, itemSchema, [...row.path, i], data, 1)}
      <div class="item" data-field="{field}[{i}]" class:bad={problemsAt(problems, `${field}[${i}]`).some((p) => p.severity === 'error')}>
        <div class="item-head">
          <span class="mono iname">{(it as Record<string, unknown>)?.name ?? `#${i + 1}`}</span>
          {#if !ro}
            <button class="btn ghost small" title="Remove" onclick={() => ondelete([...row.path, i])}><Icon name="trash" size={13} /></button>
          {/if}
        </div>
        <div class="item-fields">
          {#each sub as s (formatPath(s.row.path))}
            <FormRow row={s.row} {root} {data} {problems} {readOnly} {changeId} {secrets} inline {onset} {onappend} {ondelete} />
          {/each}
        </div>
      </div>
    {:else}
      <div class="muted small empty">No {row.label.toLowerCase()} yet.</div>
    {/each}
    {#if !ro}
      <button class="btn small add" onclick={addItem}><Icon name="plus" size={13} /> Add {row.key === 'ports' ? 'port' : 'item'}</button>
    {/if}
  </div>
{:else if inline}
  <div class="cell" data-field={field} class:wide={row.kind === 'port' || row.kind === 'rack'} title={row.help}>
    <span class="clabel">{row.label}</span>
    {@render control()}
    {#if errText && row.kind !== 'port'}<span class="err">{errText}</span>{/if}
    {@render portErr()}
  </div>
{:else}
  <div class="row-wrap" data-field={field}>
    <div class="label" title={row.help}>
      <span>{row.label}{#if row.required}<span class="req" title="required">*</span>{/if}</span>
      <span class="path mono">{field}</span>
      {#if row.owner}<span class="truth" title="owned by {row.owner} ({row.truth})">{row.owner}</span>{:else if row.truth && row.truth !== 'intent'}<span class="truth">{row.truth}</span>{/if}
    </div>
    <div class="ctl">
      {@render control()}
      {#if errText && !(row.kind === 'port')}<span class="err" class:warnt={!bad}>{errText}</span>{/if}
      {@render portErr()}
      {#if secret?.key}<span class="muted small">key: {secret.key}</span>{/if}
    </div>
  </div>
{/if}

<style>
  .row-wrap {
    display: grid;
    grid-template-columns: minmax(130px, 220px) minmax(0, 460px);
    gap: 4px 24px;
    align-items: start;
    padding: 7px 0;
  }
  /* Narrow form (projector zoom, side panel open): label above the input. */
  @container (max-width: 560px) {
    .row-wrap {
      grid-template-columns: minmax(0, 1fr);
      gap: 4px;
      padding: 6px 0;
    }
    .label {
      flex-direction: row;
      flex-wrap: wrap;
      align-items: baseline;
      gap: 0 10px;
      padding-top: 0;
    }
    .truth {
      align-self: center;
    }
  }
  .label {
    display: flex;
    flex-direction: column;
    font-size: 14.5px;
    padding-top: 6px;
    min-width: 0;
  }
  .req {
    color: var(--accent);
    margin-left: 2px;
  }
  .path {
    font-size: 11.5px;
    color: var(--muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .truth {
    align-self: flex-start;
    font-size: 10.5px;
    font-weight: 600;
    padding: 0 5px;
    margin-top: 2px;
    border-radius: 3px;
    background: var(--info-soft);
    color: var(--info);
  }
  .ctl {
    display: grid;
    gap: 3px;
    min-width: 0;
  }
  .input {
    width: 100%;
    padding: 7px 12px;
    font-size: 13.5px;
    border-radius: 6px;
  }
  .input[readonly] {
    background: var(--panel-2);
    color: var(--muted);
  }
  .input:focus,
  .picker:focus-within {
    outline: none;
    border-color: var(--accent-fill);
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--accent-fill) 30%, transparent);
  }
  .bad,
  .input.bad {
    border-color: var(--bad) !important;
    box-shadow: 0 0 0 3px color-mix(in srgb, var(--bad) 15%, transparent);
  }
  .num {
    max-width: 140px;
  }
  .with-unit {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .unit {
    color: var(--muted);
    font-size: 12.5px;
  }
  .hinted {
    position: relative;
    display: flex;
    align-items: center;
  }
  .hinted .input {
    padding-right: 84px;
  }
  .hinted .tag {
    position: absolute;
    right: 10px;
    pointer-events: none;
  }
  .tag {
    font-size: 12px;
    color: var(--muted);
    white-space: nowrap;
  }
  .tag.fit {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .tag.ok {
    color: var(--ok);
  }
  .tag.badt {
    color: var(--bad);
    font-weight: 600;
  }
  .secret {
    gap: 8px;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    padding: 7px 12px;
    color: var(--muted);
    background: var(--panel-2);
  }
  .secret .tag {
    position: static;
    margin-left: auto;
    font-weight: 600;
    color: var(--ink);
  }
  .dots {
    letter-spacing: 0.1em;
  }
  .picker {
    display: flex;
    align-items: center;
    gap: 6px;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    padding: 4px 10px;
    background: var(--panel);
    font-size: 13.5px;
    min-width: 0;
  }
  .bare {
    border: 0;
    background: transparent;
    padding: 3px 2px;
    font-size: 13.5px;
    outline: none;
  }
  .num2 {
    width: 56px;
  }
  .sep {
    color: var(--muted);
  }
  .toggle {
    position: relative;
    display: inline-flex;
    width: 44px;
    height: 24px;
    margin-top: 4px;
  }
  .toggle input {
    position: absolute;
    opacity: 0;
    inset: 0;
    margin: 0;
    cursor: pointer;
  }
  .toggle .track {
    width: 44px;
    height: 24px;
    border-radius: 12px;
    background: var(--line-strong);
    position: relative;
    transition: background 0.15s;
  }
  .toggle .track::after {
    content: '';
    position: absolute;
    top: 3px;
    left: 3px;
    width: 18px;
    height: 18px;
    border-radius: 50%;
    background: #fff;
    transition: transform 0.15s;
  }
  .toggle input:checked + .track {
    background: var(--ok);
  }
  .toggle input:checked + .track::after {
    transform: translateX(20px);
  }
  .toggle input:focus-visible + .track {
    outline: 2px solid var(--accent-fill);
    outline-offset: 2px;
  }
  .err {
    color: var(--bad);
    font-size: 12.5px;
  }
  .err.warnt {
    color: var(--warn);
  }
  .porterr {
    font-weight: 600;
    line-height: 1.4;
  }
  .porterr a {
    color: inherit;
    text-decoration: underline;
    text-underline-offset: 2px;
  }
  .tag.icon {
    display: inline-flex;
    flex: none;
  }
  .map {
    display: grid;
    grid-template-columns: minmax(80px, 160px) minmax(0, 1fr) auto;
    gap: 6px;
    align-items: center;
  }
  .k {
    font-size: 12.5px;
  }
  /* lists */
  .list {
    grid-column: 1 / -1;
    display: grid;
    gap: 8px;
    padding: 4px 0 8px;
  }
  .item {
    border: 1px solid var(--line);
    border-radius: var(--radius-s);
    padding: 8px 12px 10px;
    display: grid;
    grid-template-columns: 70px minmax(0, 1fr);
    gap: 8px;
    align-items: start;
  }
  .item-head {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    padding-top: 18px;
  }
  .iname {
    font-weight: 600;
    font-size: 14px;
  }
  .item-fields {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 10px;
    min-width: 0;
  }
  .cell {
    display: grid;
    gap: 2px;
    align-content: start;
    min-width: 92px;
    flex: 1 1 110px;
  }
  .cell.wide {
    flex: 2 1 220px;
  }
  .cell .input {
    padding: 5px 8px;
  }
  .cell .picker {
    padding: 2px 8px;
  }
  .clabel {
    font-size: 11.5px;
    color: var(--muted);
  }
  .empty {
    padding: 4px 0;
  }
  .add {
    justify-self: start;
  }
</style>
