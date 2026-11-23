<script lang="ts">
  // Semantic diff lines ("cn-003 p2: data-b 37 to data-a 37") from kind hooks.
  import type { SemanticLine } from '../api/ftypes';

  let {
    lines,
    inline = false,
    fileHref,
  }: { lines: SemanticLine[]; inline?: boolean; fileHref?: (l: SemanticLine) => string | undefined } = $props();

  const sym = (op: string) => (op === 'add' ? '+' : op === 'delete' ? '−' : '~');
</script>

{#if inline}
  <span class="sem inline">
    {#each lines as l, i (i)}
      <span class="item"><span class="op op-{l.op}">{sym(l.op)}</span> {l.subject}{l.text ? ` ${l.text}` : ''}</span>
    {/each}
  </span>
{:else}
  <ul class="sem">
    {#each lines as l, i (i)}
      {@const link = fileHref?.(l)}
      <li>
        <span class="op op-{l.op}" title={l.op}>{sym(l.op)}</span>
        <span class="txt"><span class="subj">{l.subject}</span>{#if l.text}: {l.text}{/if}</span>
        {#if link && l.path}
          <a class="path mono" href={link} title={l.path}>{l.path.split('/').pop()}{l.line ? `:${l.line}` : ''}</a>
        {/if}
      </li>
    {:else}
      <li class="muted small">No semantic changes.</li>
    {/each}
  </ul>
{/if}

<style>
  .sem {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 4px;
  }
  .sem li {
    display: flex;
    gap: 8px;
    align-items: baseline;
  }
  .txt {
    flex: 1;
    min-width: 0;
  }
  .subj {
    font-weight: 600;
  }
  .op {
    font: 700 13px/1 var(--mono);
    width: 12px;
    flex: none;
    text-align: center;
  }
  .op-add {
    color: var(--ok);
  }
  .op-change {
    color: var(--warn);
  }
  .op-delete {
    color: var(--bad);
  }
  .path {
    font-size: 11.5px;
    color: var(--muted);
    text-decoration: none;
  }
  .path:hover {
    color: var(--accent);
  }
  .inline {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 4px 18px;
  }
  .inline .item {
    white-space: nowrap;
  }
</style>
