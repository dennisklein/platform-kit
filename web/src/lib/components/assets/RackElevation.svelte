<script lang="ts" module>
  import type { RackOccupant } from '../../api/ftypes';

  /** Color class of an occupant: data/mgmt switches, compute, storage, ... */
  export function occClass(o: RackOccupant): string {
    if (o.kind === 'Switch') return o.name.startsWith('mgmt') ? 'mgmt' : 'data';
    if (o.kind === 'Jbod' || o.category === 'jbod') return 'jbod';
    return o.category ?? 'other';
  }
</script>

<script lang="ts">
  // Rack elevation: units from the top (47) to the bottom (1), occupants as
  // blocks colored by role, highlights of the change (new, changed, gone).
  import type { Rack } from '../../api/ftypes';

  let {
    rack,
    selected,
    unitPx = 15,
    compact = false,
    onselect,
    linkOf,
  }: {
    rack: Rack;
    selected?: string;
    unitPx?: number;
    /** Overview size: no unit numbers, smaller names. */
    compact?: boolean;
    onselect?: (o: RackOccupant) => void;
    linkOf?: (o: RackOccupant) => string;
  } = $props();

  const units = $derived(rack.units || 47);
  const top = (o: RackOccupant) => (units - (o.unit + o.size - 1)) * unitPx;
  const occupied = $derived(new Set(rack.occupants.flatMap((o) => Array.from({ length: o.size }, (_, i) => o.unit + i))));
  const legend = $derived(
    [
      ['data', 'data'],
      ['mgmt', 'mgmt'],
      ['compute', 'compute'],
      ['storage', 'storage'],
      ['control', 'control'],
      ['jbod', 'JBOD'],
    ].filter(([c]) => rack.occupants.some((o) => occClass(o) === c)),
  );
  // What the change marks mean, for the occupants this rack has.
  const marks = $derived(
    [
      ['added', '+', 'new'],
      ['changed', '~', 'changed'],
      ['deleted', '−', 'removed'],
      ['conflict', '!', 'conflict'],
    ].filter(([h]) => rack.occupants.some((o) => o.highlight === h)),
  );
  const changeOf = $derived(rack.occupants.find((o) => o.highlight && o.highlight !== 'none' && o.changeId)?.changeId);
  const hasProblems = $derived(rack.occupants.some((o) => (o.problems?.length ?? 0) > 0));
  const label = (o: RackOccupant) =>
    o.highlight === 'added' ? '+' : o.highlight === 'changed' ? '~' : o.highlight === 'deleted' ? '−' : o.highlight === 'conflict' ? '!' : '';
</script>

<div class="rackwrap" class:compact>
  <div class="frame" style:height="{units * unitPx + 22}px">
    <div class="units" aria-hidden="true">
      {#each Array.from({ length: units }, (_, i) => units - i) as u (u)}
        <span class="u" class:free={!occupied.has(u)} style:height="{unitPx}px">{!compact && (u % 2 === 0 || unitPx >= 18) ? u : ''}</span>
      {/each}
    </div>
    <div class="slots" style:height="{units * unitPx}px" style:--u="{unitPx}px">
      {#each rack.occupants as o (o.kind + o.name + o.unit)}
        {@const h = o.size * unitPx - 2}
        <svelte:element
          this={linkOf ? 'a' : 'button'}
          href={linkOf?.(o)}
          type={linkOf ? undefined : 'button'}
          role={linkOf ? 'link' : 'button'}
          class="occ c-{occClass(o)} h-{o.highlight ?? 'none'}"
          class:sel={selected === o.name}
          class:thin={h < 13}
          class:bad={(o.problems?.length ?? 0) > 0}
          style:top="{top(o)}px"
          style:height="{h}px"
          title="{o.kind} {o.name} · U{o.unit}{o.size > 1 ? `–${o.unit + o.size - 1}` : ''}{o.role ? ` · ${o.role}` : ''}{o.cluster
            ? ` · ${o.cluster}`
            : ''}{o.highlight && o.highlight !== 'none' ? ` · ${o.highlight} in ${o.changeId ?? 'change'}` : ''}{o.problems?.length
            ? `\n${o.problems.join('\n')}`
            : ''}"
          onclick={() => onselect?.(o)}
          aria-current={selected === o.name ? 'true' : undefined}
        >
          {#if h >= 9}<span class="nm">{o.name}</span>{/if}
          {#if o.problems?.length}<span class="warn" aria-label="problems">!</span>{/if}
          {#if label(o)}<span class="mk">{label(o)}</span>{/if}
        </svelte:element>
      {/each}
    </div>
  </div>
  {#if legend.length || marks.length || hasProblems}
    <div class="legend">
      {#each legend as [c, l] (c)}
        <span><i class="sw c-{c}"></i>{l}</span>
      {/each}
      {#each marks as [h, m, l] (h)}
        <span class="lm lm-{h}"><b>{m}</b>{l}{h === 'added' && changeOf ? ` in ${changeOf}` : ''}</span>
      {/each}
      {#if hasProblems && !marks.some(([h]) => h === 'conflict')}<span class="lm lm-conflict"><b>!</b>problem</span>{/if}
    </div>
  {/if}
</div>

<style>
  .rackwrap {
    display: grid;
    gap: 8px;
  }
  .frame {
    display: grid;
    grid-template-columns: 28px 1fr;
    background: #1d2026;
    border-radius: 6px;
    padding: 12px 10px 8px 4px;
    border: 1px solid #0d0f12;
    box-shadow: inset 0 0 0 3px #2a2e36;
  }
  .units {
    display: flex;
    flex-direction: column;
  }
  /* Unit numbers readable on a projector ("cn-035 in U6"). */
  .u {
    font: 600 11px/1 var(--mono);
    color: #9aa3b2;
    display: flex;
    align-items: center;
    justify-content: flex-end;
    padding-right: 4px;
  }
  .slots {
    position: relative;
    background: repeating-linear-gradient(to bottom, #262a31 0 1px, transparent 1px 100%);
    background-size: 100% var(--u, 15px);
  }
  .occ {
    position: absolute;
    left: 2px;
    right: 2px;
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 0 8px;
    border: 1px solid transparent;
    border-radius: 2px;
    font: 600 12.5px/1 var(--mono);
    color: #eef2f7;
    text-decoration: none;
    text-align: left;
    cursor: pointer;
    overflow: hidden;
  }
  .occ:hover {
    filter: brightness(1.15);
  }
  .nm {
    flex: 1;
    white-space: nowrap;
  }
  .mk {
    font-weight: 700;
  }
  .warn {
    display: inline-grid;
    place-items: center;
    width: 14px;
    height: 14px;
    border-radius: 50%;
    background: #ff8a80;
    color: #3d1c1a;
    font-weight: 700;
    font-size: 10px;
  }
  .c-compute {
    background: #0b5cad;
  }
  .c-storage {
    background: var(--rack-storage);
  }
  .c-control {
    background: #6a4fb3;
  }
  .c-data {
    background: #d4473b;
  }
  .c-mgmt {
    background: #b0651a;
  }
  .c-jbod {
    background: #5b6270;
  }
  .c-network,
  .c-other {
    background: #4a5260;
  }
  .h-added {
    background: #2c5a26;
    border: 1.5px solid #7fd36a;
    color: #e6ffe0;
  }
  .h-changed {
    border: 1.5px solid #f2c14e;
  }
  .h-deleted {
    background: transparent;
    border: 1.5px dashed #ff8a80;
    color: #ff8a80;
    text-decoration: line-through;
  }
  .h-conflict,
  .bad {
    border: 1.5px solid #ff8a80;
  }
  .sel {
    outline: 2px solid var(--gsi-orange);
    outline-offset: 1px;
    z-index: 1;
  }
  .compact .frame {
    grid-template-columns: 6px 1fr;
    padding: 8px 8px 8px 4px;
  }
  .compact .occ {
    font-size: 10.5px;
    padding: 0 6px;
    gap: 4px;
  }
  /* A 1U device in the compact view is 10 px high: a line box as high as
     the device, and a clip margin, keep the descenders of "mgmt" and
     "cp-03" whole. */
  .occ.thin {
    font-size: 9px;
    line-height: 10px;
    overflow: clip;
    overflow-clip-margin: 2px;
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 12px;
    font-size: 12.5px;
    color: var(--muted);
  }
  .lm b {
    font: 700 13px var(--mono);
    margin-right: 4px;
  }
  .lm-added b {
    color: var(--ok);
  }
  .lm-changed b {
    color: var(--warn);
  }
  .lm-deleted b,
  .lm-conflict b {
    color: var(--bad);
  }
  .legend span {
    display: inline-flex;
    align-items: center;
    gap: 5px;
  }
  .sw {
    width: 10px;
    height: 10px;
    border-radius: 2px;
    display: inline-block;
  }
</style>
