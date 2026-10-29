<script lang="ts">
  // JBOD drawers as a slot grid (ok, replace, empty).
  let {
    drawers,
  }: { drawers: { id: number; slots: { slot: number; state: string; serial?: string }[] }[] } = $props();
</script>

<div class="drawers">
  {#each drawers as d (d.id)}
    <div class="drawer">
      <span class="lbl mono">drawer {d.id}</span>
      <div class="slots">
        {#each d.slots as s (s.slot)}
          <span class="slot st-{s.state}" title="drawer {d.id} slot {s.slot}: {s.state}{s.serial ? ` · ${s.serial}` : ''}">{s.slot}</span>
        {/each}
      </div>
    </div>
  {/each}
  <div class="legend small muted">
    <span><i class="slot st-ok"></i> ok</span>
    <span><i class="slot st-replace"></i> replace</span>
    <span><i class="slot st-empty"></i> empty</span>
  </div>
</div>

<style>
  .drawers {
    display: grid;
    gap: 8px;
  }
  .drawer {
    display: grid;
    grid-template-columns: 70px 1fr;
    gap: 8px;
    align-items: center;
  }
  .lbl {
    font-size: 11.5px;
    color: var(--muted);
  }
  .slots {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(26px, 1fr));
    gap: 3px;
  }
  .slot {
    display: inline-grid;
    place-items: center;
    height: 22px;
    min-width: 22px;
    border-radius: 3px;
    font: 500 10px/1 var(--mono);
    background: var(--ok-soft);
    color: var(--ok);
    border: 1px solid color-mix(in srgb, var(--ok) 30%, transparent);
  }
  .st-replace {
    background: var(--warn-soft);
    color: var(--warn);
    border-color: var(--warn);
  }
  .st-empty {
    background: transparent;
    color: var(--muted);
    border-style: dashed;
    border-color: var(--line-strong);
  }
  .legend {
    display: flex;
    gap: 14px;
  }
  .legend span {
    display: inline-flex;
    gap: 5px;
    align-items: center;
  }
  .legend .slot {
    height: 10px;
    min-width: 10px;
  }
</style>
