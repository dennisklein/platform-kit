<script lang="ts" module>
  export type TrackStatus = 'done' | 'now' | 'fail' | 'todo' | 'running' | 'skipped';
  export interface TrackItem {
    key: string;
    title: string;
    sub?: string;
    status: TrackStatus;
    detail?: string;
  }
</script>

<script lang="ts">
  // Horizontal steps with dots and lines (change lifecycle, lab stages).
  let {
    items,
    selected = $bindable<string | undefined>(undefined),
    selectable = false,
  }: { items: TrackItem[]; selected?: string; selectable?: boolean } = $props();
</script>

<div class="trackbox">
<ol class="track" style:--n={items.length}>
  {#each items as it, i (it.key)}
    <li class="st-{it.status}" class:sel={selectable && selected === it.key} class:next-done={items[i + 1]?.status === 'done' || items[i + 1]?.status === 'fail' || items[i + 1]?.status === 'now' || items[i + 1]?.status === 'running'}>
      {#if selectable}
        <button
          class="hit"
          title={it.detail ?? it.title}
          aria-pressed={selected === it.key}
          onclick={() => (selected = selected === it.key ? undefined : it.key)}
        >
          <span class="dot" aria-hidden="true"></span>
          <span class="t">{it.title}</span>
          <span class="s">{it.sub || ' '}</span>
        </button>
      {:else}
        <div class="hit" title={it.detail ?? it.title}>
          <span class="dot" aria-hidden="true"></span>
          <span class="t">{it.title}</span>
          <span class="s">{it.sub || ' '}</span>
        </div>
      {/if}
    </li>
  {/each}
</ol>
</div>

<style>
  /* Labels wrap to two lines instead of being cut; below 60 px a step the
     track scrolls sideways. Narrow tracks use a smaller font. */
  .trackbox {
    container-type: inline-size;
    overflow-x: auto;
    scrollbar-width: thin;
    /* room for the ring of the running dot (the box clips its overflow) */
    padding: 8px 4px 2px;
    margin: -8px -4px -2px;
  }
  .track {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    grid-template-columns: repeat(var(--n), minmax(60px, 1fr));
  }
  li {
    position: relative;
    min-width: 0;
  }
  /* line to the next dot */
  li:not(:last-child)::after {
    content: '';
    position: absolute;
    top: 10px;
    left: calc(50% + 10px);
    right: calc(-50% + 10px);
    height: 2px;
    background: var(--line-strong);
    border-radius: 1px;
  }
  li.st-done.next-done::after {
    background: var(--ok);
  }
  .hit {
    width: 100%;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    border: 0;
    background: none;
    padding: 0 2px 2px;
    color: inherit;
    font: inherit;
    border-radius: 6px;
  }
  button.hit:hover .t {
    text-decoration: underline;
    text-decoration-color: var(--line-strong);
  }
  .sel .hit {
    background: var(--panel-2);
  }
  .dot {
    width: 18px;
    height: 18px;
    margin: 2px 0 4px;
    border-radius: 50%;
    border: 2px solid var(--line-strong);
    background: var(--panel);
    box-sizing: border-box;
    position: relative;
    z-index: 1;
  }
  .t,
  .s {
    max-width: 100%;
    text-align: center;
    line-height: 1.25;
    overflow-wrap: break-word;
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    overflow: hidden;
  }
  .t {
    font-weight: 600;
    font-size: 13.5px;
    color: var(--muted);
    -webkit-line-clamp: 3;
    line-clamp: 3;
  }
  .s {
    font-size: 12px;
    color: var(--muted);
  }
  @container (max-width: 760px) {
    .t {
      font-size: 12.5px;
    }
    .s {
      font-size: 11.5px;
    }
  }
  .st-done .dot {
    background: var(--ok);
    border-color: var(--ok);
  }
  .st-done .t {
    color: var(--ink);
  }
  .st-now .dot,
  .st-running .dot {
    background: var(--accent-fill);
    border-color: var(--accent-fill);
    box-shadow: 0 0 0 4px color-mix(in srgb, var(--accent-fill) 30%, transparent);
  }
  .st-running .dot {
    animation: ring 1.4s ease-in-out infinite;
  }
  @keyframes ring {
    50% {
      box-shadow: 0 0 0 7px color-mix(in srgb, var(--accent-fill) 10%, transparent);
    }
  }
  .st-now .t,
  .st-running .t {
    color: var(--accent);
  }
  .st-fail .dot {
    background: var(--bad);
    border-color: var(--bad);
  }
  .st-fail .t {
    color: var(--bad);
  }
  .st-skipped .dot {
    border-style: dashed;
  }
</style>
