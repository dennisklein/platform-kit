<script lang="ts">
  import { toasts } from '../stores/toasts.svelte';
  import Icon from './Icon.svelte';

  const ICON = { ok: 'check', bad: 'warning', info: 'info', warn: 'warning' } as const;
</script>

<div class="toasts" aria-live="polite">
  {#each toasts.list as t (t.id)}
    <div class="toast t-{t.tone}" role={t.tone === 'bad' ? 'alert' : 'status'}>
      <span class="ic"><Icon name={ICON[t.tone]} size={16} /></span>
      <div class="msg">
        <div>{t.text}</div>
        {#if t.detail}<div class="detail">{t.detail}</div>{/if}
        {#if t.link}<a href={t.link.href} target="_blank" rel="noreferrer">{t.link.label} <Icon name="external" size={12} /></a>{/if}
      </div>
      <button class="btn ghost small" onclick={() => toasts.dismiss(t.id)} title="Dismiss"><Icon name="close" size={12} /></button>
    </div>
  {/each}
</div>

<style>
  .toasts {
    position: fixed;
    right: 16px;
    bottom: 16px;
    display: grid;
    gap: 8px;
    z-index: 60;
    width: min(420px, calc(100vw - 32px));
  }
  .toast {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    background: var(--panel);
    border: 1px solid var(--line);
    border-left: 3px solid var(--info);
    border-radius: var(--radius-s);
    box-shadow: var(--shadow);
    padding: 10px 8px 10px 12px;
    font-size: 13px;
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
  .detail {
    color: var(--muted);
    font-size: 12.5px;
    overflow-wrap: anywhere;
  }
  a {
    font-size: 12.5px;
    display: inline-flex;
    align-items: center;
    gap: 3px;
  }
</style>
