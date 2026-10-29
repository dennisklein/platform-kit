<script lang="ts">
  // Modal dialog with a dimmed backdrop. Escape and backdrop click close it.
  import type { Snippet } from 'svelte';
  import { fade, scale } from 'svelte/transition';
  import { cubicOut } from 'svelte/easing';
  import { ms } from '../f/motion';
  import Icon from './Icon.svelte';

  let {
    open = $bindable(false),
    title,
    icon,
    width = 640,
    onclose,
    children,
    footer,
  }: {
    open?: boolean;
    title: string;
    icon?: string;
    width?: number;
    onclose?: () => void;
    children: Snippet;
    footer?: Snippet;
  } = $props();

  let dialog = $state<HTMLDivElement>();

  function close() {
    open = false;
    onclose?.();
  }

  $effect(() => {
    if (!open) return;
    const prev = document.activeElement as HTMLElement | null;
    queueMicrotask(() => {
      const first = dialog?.querySelector<HTMLElement>('[autofocus], input, textarea, select, button.primary');
      first?.focus();
    });
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close();
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
      prev?.focus?.();
    };
  });
</script>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={(e) => e.target === e.currentTarget && close()} transition:fade={{ duration: ms(140) }}>
    <div
      class="dialog"
      role="dialog"
      aria-modal="true"
      aria-label={title}
      bind:this={dialog}
      style:max-width="{width}px"
      in:scale={{ start: 0.96, duration: ms(180), easing: cubicOut }}
    >
      <header>
        {#if icon}<span class="ic"><Icon name={icon} size={20} /></span>{/if}
        <h2>{title}</h2>
        <span class="spacer"></span>
        <button class="btn ghost small" onclick={close} title="Close"><Icon name="close" size={14} /></button>
      </header>
      <div class="body">{@render children()}</div>
      {#if footer}<footer>{@render footer()}</footer>{/if}
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgb(19 21 25 / 0.42);
    display: grid;
    place-items: center;
    z-index: 50;
    padding: 16px;
  }
  .dialog {
    width: 100%;
    background: var(--panel);
    border-radius: 10px;
    box-shadow: 0 20px 60px rgb(0 0 0 / 0.3);
    display: flex;
    flex-direction: column;
    max-height: calc(100vh - 32px);
  }
  header {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 18px 22px 8px;
  }
  .ic {
    color: var(--accent);
    display: inline-flex;
  }
  .body {
    padding: 8px 22px 12px;
    overflow: auto;
  }
  footer {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding: 10px 22px 18px;
  }
</style>
