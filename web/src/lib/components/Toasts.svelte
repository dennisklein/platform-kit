<script lang="ts">
  // Toasts sit top right, under the top bar: away from the editor status
  // line, the gates card and the advisor buttons the demo points at.
  import { flip } from 'svelte/animate';
  import { fade, fly } from 'svelte/transition';
  import { cubicOut } from 'svelte/easing';
  import { toasts } from '../stores/toasts.svelte';
  import { ms } from '../f/motion';
  import Icon from './Icon.svelte';

  const ICON = { ok: 'check', bad: 'warning', info: 'info', warn: 'warning' } as const;
  /** Good news goes away sooner than the store default; errors stay. */
  const SHORT_MS = 4000;

  const timers = new Map<number, ReturnType<typeof setTimeout>>();
  $effect(() => {
    const ids = new Set(toasts.list.map((t) => t.id));
    for (const t of toasts.list) {
      if ((t.tone === 'ok' || t.tone === 'info') && !timers.has(t.id)) {
        timers.set(
          t.id,
          setTimeout(() => {
            timers.delete(t.id);
            toasts.dismiss(t.id);
          }, SHORT_MS),
        );
      }
    }
    for (const [id, timer] of timers) {
      if (!ids.has(id)) {
        clearTimeout(timer);
        timers.delete(id);
      }
    }
  });
</script>

<div class="toasts" aria-live="polite">
  {#each toasts.list as t (t.id)}
    <div
      class="toast t-{t.tone}"
      role={t.tone === 'bad' ? 'alert' : 'status'}
      animate:flip={{ duration: ms(200) }}
      in:fly={{ y: -10, duration: ms(200), easing: cubicOut }}
      out:fade={{ duration: ms(140) }}
    >
      <span class="ic"><Icon name={ICON[t.tone]} size={16} /></span>
      <div class="msg">
        <div class="text">{t.text}</div>
        {#if t.detail}<div class="detail" class:full={t.tone === 'bad'} title={t.detail}>{t.detail}</div>{/if}
        {#if t.link}<a href={t.link.href} target="_blank" rel="noreferrer">{t.link.label} <Icon name="external" size={12} /></a>{/if}
      </div>
      <button class="btn ghost small" onclick={() => toasts.dismiss(t.id)} title="Dismiss"><Icon name="close" size={12} /></button>
    </div>
  {/each}
</div>

<style>
  .toasts {
    position: fixed;
    top: calc(var(--topbar-h) + 10px);
    right: 16px;
    display: grid;
    gap: 8px;
    z-index: 60;
    width: min(380px, calc(100vw - 32px));
    pointer-events: none;
  }
  .toast {
    pointer-events: auto;
    display: flex;
    gap: 10px;
    align-items: flex-start;
    background: var(--panel);
    border: 1px solid var(--line);
    border-left: 4px solid var(--info);
    border-radius: var(--radius-s);
    box-shadow: 0 2px 4px rgb(0 0 0 / 0.08), 0 10px 28px rgb(0 0 0 / 0.14);
    padding: 9px 6px 9px 12px;
    font-size: 13.5px;
  }
  .t-ok {
    border-left-color: var(--ok);
  }
  .t-ok .ic {
    color: var(--ok);
  }
  .t-bad {
    border-left-color: var(--bad);
  }
  .t-bad .ic {
    color: var(--bad);
  }
  .t-info .ic {
    color: var(--info);
  }
  .t-warn {
    border-left-color: var(--warn);
  }
  .t-warn .ic {
    color: var(--warn);
  }
  .ic {
    display: inline-flex;
    padding-top: 2px;
  }
  .msg {
    flex: 1;
    min-width: 0;
    display: grid;
    gap: 2px;
  }
  .text {
    font-weight: 600;
  }
  /* Short: one line of detail (long paths are in the tooltip). Errors show
     all of it. */
  .detail {
    color: var(--muted);
    font-size: 12.5px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .detail.full {
    white-space: normal;
    overflow-wrap: anywhere;
  }
  a {
    font-size: 12.5px;
    display: inline-flex;
    align-items: center;
    gap: 3px;
  }
</style>
