<script lang="ts">
  // A unified diff (text from the API) with colored lines.
  import { parseUnified } from '../f/linediff';

  let {
    diff,
    headers = false,
    maxLines = 400,
  }: { diff: string; headers?: boolean; maxLines?: number } = $props();

  const lines = $derived(parseUnified(diff ?? '').filter((l) => headers || l.op !== 'h'));
  const shown = $derived(lines.slice(0, maxLines));
</script>

<pre class="udiff">{#each shown as l, i (i)}<span class="ln op-{l.op === ' ' ? 'ctx' : l.op === '+' ? 'add' : l.op === '-' ? 'del' : l.op === '@' ? 'hunk' : 'head'}"><span class="mark" aria-hidden="true">{l.op === '@' || l.op === 'h' || l.op === ' ' ? ' ' : l.op}</span>{l.text || ' '}</span>{/each}{#if lines.length > shown.length}<span class="ln op-hunk"> … {lines.length - shown.length} more lines</span>{/if}</pre>

<style>
  .udiff {
    padding: 6px 0;
    font-size: 12px;
    line-height: 1.55;
    overflow-x: auto;
  }
  .ln {
    display: block;
    padding: 0 12px 0 0;
    white-space: pre;
    min-width: max-content;
  }
  .mark {
    display: inline-block;
    width: 22px;
    text-align: center;
    color: var(--muted);
    user-select: none;
  }
  .op-add {
    background: var(--ok-soft);
  }
  .op-add .mark {
    color: var(--ok);
  }
  .op-del {
    background: var(--bad-soft);
  }
  .op-del .mark {
    color: var(--bad);
  }
  .op-hunk,
  .op-head {
    color: var(--muted);
    padding-left: 22px;
  }
</style>
