<script lang="ts">
  // "dennis-agent works here": an agent works on this change right now.
  import type { Presence } from '../api/types';
  import { ui } from '../stores/ui.svelte';
  import { timeAgo } from '../change/format';
  import Icon from './Icon.svelte';

  let { presence, compact = false }: { presence: Presence; compact?: boolean } = $props();
  const who = $derived(presence.actsFor ? `${presence.actsFor}/agent` : presence.actor);
</script>

<button
  class="presence"
  class:compact
  title="{presence.actor}: {presence.text ?? presence.tool ?? 'working'} ({timeAgo(presence.at)})"
  onclick={() => ui.openSide('activity')}
>
  <span class="bot"><Icon name="bot" size={compact ? 12 : 15} /></span>
  {#if compact}agent here{:else}{who} works here{/if}
</button>

<style>
  .presence {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    border: 0;
    border-radius: 999px;
    padding: 4px 14px 4px 4px;
    background: var(--info-soft);
    color: var(--info);
    font-weight: 600;
    font-size: 13.5px;
    white-space: nowrap;
    box-shadow: 0 0 0 0 color-mix(in srgb, var(--info) 35%, transparent);
    animation: here 2.4s ease-out infinite;
  }
  .compact {
    font-size: 12px;
    padding: 2px 9px 2px 3px;
    gap: 5px;
  }
  .bot {
    display: inline-grid;
    place-items: center;
    width: 26px;
    height: 26px;
    border-radius: 50%;
    background: var(--info);
    color: var(--panel);
  }
  .compact .bot {
    width: 18px;
    height: 18px;
  }
  @keyframes here {
    0% {
      box-shadow: 0 0 0 0 color-mix(in srgb, var(--info) 35%, transparent);
    }
    70%,
    100% {
      box-shadow: 0 0 0 8px transparent;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .presence {
      animation: none;
    }
  }
</style>
