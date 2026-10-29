<script lang="ts">
  // A commit message or MR description with its trailer block, styled as
  // on the history page: keys in colour, ids as links. A trailer whose
  // value changes (an approval arrives) lights up for a moment.
  import { untrack } from 'svelte';
  import { href } from '../../lib/router.svelte';
  import type { MessageParts } from './trailers';

  let { parts, label = 'Trailers' }: { parts: MessageParts; label?: string } = $props();

  let flash = $state<Record<string, boolean>>({});
  let last: Record<string, string> | null = null;
  let lastSubject = '';
  const timers: ReturnType<typeof setTimeout>[] = [];
  $effect(() => {
    const now: Record<string, string> = {};
    for (const [k, v] of parts.trailers) now[k] = now[k] ? `${now[k]}\n${v}` : v;
    // Another message (the view switched) is not a change: no flash.
    if (last && parts.subject === lastSubject) {
      const changed = Object.keys(now).filter((k) => last![k] !== now[k]);
      if (changed.length) {
        untrack(() => (flash = { ...flash, ...Object.fromEntries(changed.map((k) => [k, true])) }));
        timers.push(
          setTimeout(() => {
            const next = { ...flash };
            for (const k of changed) delete next[k];
            flash = next;
          }, 1800),
        );
      }
    }
    last = now;
    lastSubject = parts.subject;
  });
  $effect(() => () => timers.forEach(clearTimeout));

  const PENDING = /^\(.*\)$/;
</script>

<div class="msg">
  {#if parts.subject}<div class="subject">{parts.subject}</div>{/if}
  {#each parts.body as b, i (i)}<p class="body">{b}</p>{/each}
  {#if parts.trailers.length}
    <section class="trailers">
      <div class="th">{label}</div>
      <dl>
        {#each parts.trailers as [k, v], i (i)}
          <div class:flash={flash[k]}>
            <dt>{k}:</dt>
            <dd class:pending={PENDING.test(v)}>
              {#if k === 'Pk-Change'}
                <a href={href(`/changes/${v}`)}>{v}</a>
              {:else if k === 'Pk-Lab-Run'}
                {@const f = v.split(' ')}
                <a href={href(`/labs/runs/${encodeURIComponent(f[0])}`)}>{f[0]}</a>
                <span class="res r-{f[1]}">{f[1] ?? ''}</span>
                {#if f[2]}<a href={href('/kb/article', { path: f[2], ref: v.split('/')[0] })}>{f[2]}</a>{/if}
              {:else if k === 'Pk-Work-Item'}
                <a href={href('/work', { item: v })}>{v}</a>
              {:else}
                {v}
              {/if}
            </dd>
          </div>
        {/each}
      </dl>
    </section>
  {/if}
</div>

<style>
  .msg {
    display: grid;
    gap: 8px;
  }
  .subject {
    font: 600 13.5px/1.5 var(--mono);
    color: var(--ink);
  }
  .body {
    margin: 0;
    font-size: 14px;
    color: var(--muted);
    white-space: pre-wrap;
  }
  .trailers {
    border-left: 4px solid var(--accent-fill);
    background: var(--code-bg);
    padding: 10px 14px;
    border-radius: 0 var(--radius-s) var(--radius-s) 0;
  }
  .th {
    font: 600 11.5px var(--mono);
    letter-spacing: 0.12em;
    text-transform: uppercase;
    color: var(--accent);
    margin-bottom: 6px;
  }
  dl {
    margin: 0;
    font: 13px/1.75 var(--mono);
  }
  dl div {
    display: flex;
    gap: 6px;
    margin: 0 -6px;
    padding: 0 6px;
    border-radius: 4px;
    transition: background-color 0.6s ease;
  }
  dl div.flash {
    background: color-mix(in srgb, var(--ok) 22%, transparent);
    transition: none;
  }
  dt {
    color: var(--info);
    white-space: nowrap;
  }
  dd {
    margin: 0;
    word-break: break-word;
    color: var(--ink);
  }
  dd.pending {
    color: var(--muted);
    font-style: italic;
  }
  .res {
    font-weight: 600;
  }
  .r-pass {
    color: var(--ok);
  }
  .r-fail {
    color: var(--bad);
  }
</style>
