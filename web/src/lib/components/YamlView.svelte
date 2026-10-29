<script lang="ts" module>
  export interface Seg {
    t: string;
    c?: 'k' | 'v' | 'p' | 'c' | 's' | 'n';
  }

  /** Split one YAML line into colored segments (keys, values, comments). */
  export function yamlLine(line: string): Seg[] {
    const out: Seg[] = [];
    const m = /^(\s*)(- )?(?:([\w.$/-]+|"[^"]*")(:)(?=\s|$))?(.*)$/.exec(line);
    if (!m) return [{ t: line }];
    const [, ind, dash, key, colon, rest] = m;
    if (ind) out.push({ t: ind });
    if (dash) out.push({ t: dash, c: 'p' });
    if (key) {
      out.push({ t: key, c: 'k' });
      out.push({ t: colon, c: 'p' });
    }
    let r = rest;
    let comment = '';
    const ci = r.search(/(^|\s)#/);
    if (ci >= 0 && !/^\s*["']/.test(r)) {
      comment = r.slice(ci);
      r = r.slice(0, ci);
    }
    if (r.trim().startsWith('#')) {
      out.push({ t: r, c: 'c' });
    } else if (r) {
      const v = r.trim();
      const c: Seg['c'] = /^(ENC\[|•+)/.test(v) ? 's' : /^-?\d+(\.\d+)?$|^(true|false|null)$/.test(v) ? 'n' : 'v';
      out.push({ t: r, c });
    }
    if (comment) out.push({ t: comment, c: 'c' });
    return out;
  }
</script>

<script lang="ts">
  // Read-only YAML with simple coloring and optional line numbers.
  let {
    text,
    numbers = false,
    highlight = [],
  }: { text: string; numbers?: boolean; highlight?: number[] } = $props();

  const lines = $derived((text ?? '').replace(/\n$/, '').split('\n'));
  const hl = $derived(new Set(highlight));
</script>

<pre class="yv" class:numbers>{#each lines as l, i (i)}<span class="ln" class:hl={hl.has(i + 1)}>{#if numbers}<span class="no">{i + 1}</span>{/if}{#each yamlLine(l) as s, j (j)}<span class={s.c ? `y-${s.c}` : undefined}>{s.t}</span>{/each}</span>{/each}</pre>

<style>
  .yv {
    font-variant-ligatures: none;
    font-size: 12.5px;
    line-height: 1.6;
    margin: 0;
  }
  .ln {
    display: block;
    min-height: 1.6em;
  }
  .ln.hl {
    background: var(--warn-soft);
  }
  .no {
    display: inline-block;
    width: 28px;
    margin-right: 10px;
    text-align: right;
    color: var(--muted);
    user-select: none;
  }
  .y-k {
    color: var(--info);
  }
  .y-v {
    color: var(--ok);
  }
  .y-n {
    color: var(--warn);
  }
  .y-p {
    color: var(--muted);
  }
  .y-c {
    color: var(--muted);
    font-style: italic;
  }
  .y-s {
    color: var(--bad);
  }
</style>
