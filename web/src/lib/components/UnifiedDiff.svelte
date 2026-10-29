<script lang="ts">
  // A unified diff (text from the API) with coloured lines. Long lines wrap
  // with a hanging indent; the changed words of an edited line stand out.
  import { parseUnified, wordHighlights } from '../f/linediff';

  let {
    diff,
    headers = false,
    maxLines = 400,
  }: { diff: string; headers?: boolean; maxLines?: number } = $props();

  const lines = $derived(parseUnified(diff ?? '').filter((l) => headers || l.op !== 'h'));
  const shown = $derived(lines.slice(0, maxLines));
  const words = $derived(wordHighlights(shown));
  const cls = (op: string) => (op === ' ' ? 'ctx' : op === '+' ? 'add' : op === '-' ? 'del' : op === '@' ? 'hunk' : 'head');
</script>

<pre class="udiff">{#each shown as l, i (i)}{@const segs = words.get(i)}<span class="ln op-{cls(l.op)}"><span class="mark" aria-hidden="true">{l.op === '@' || l.op === 'h' || l.op === ' ' ? ' ' : l.op}</span>{#if segs}{#each segs as s, k (k)}{#if s.changed}<mark class="w">{s.text}</mark>{:else}{s.text}{/if}{/each}{:else}{l.text || ' '}{/if}</span>{/each}{#if lines.length > shown.length}<span class="ln op-hunk"> … {lines.length - shown.length} more lines</span>{/if}</pre>

<style>
  .udiff {
    padding: 6px 0;
    font-size: 13px;
    line-height: 1.6;
    overflow-x: auto;
  }
  /* Wrap long lines; the continuation starts under the text, not under the
     +/- mark. */
  .ln {
    display: block;
    padding: 0 12px 0 26px;
    text-indent: -26px;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .mark {
    display: inline-block;
    width: 26px;
    text-indent: 0;
    text-align: center;
    color: var(--muted);
    font-weight: 700;
    user-select: none;
  }
  .op-add {
    background: color-mix(in srgb, var(--ok) 15%, var(--panel));
    box-shadow: inset 3px 0 var(--ok);
  }
  .op-add .mark {
    color: var(--ok);
  }
  .op-del {
    background: color-mix(in srgb, var(--bad) 13%, var(--panel));
    box-shadow: inset 3px 0 var(--bad);
  }
  .op-del .mark {
    color: var(--bad);
  }
  .w {
    color: inherit;
    border-radius: 3px;
    padding: 0 1px;
  }
  .op-add .w {
    background: color-mix(in srgb, var(--ok) 38%, var(--panel));
  }
  .op-del .w {
    background: color-mix(in srgb, var(--bad) 32%, var(--panel));
  }
  .op-hunk,
  .op-head {
    color: var(--muted);
  }
</style>
