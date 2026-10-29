<script lang="ts">
  // Banner on a change an agent works on: who, through what, until when.
  import type { Presence } from '../api/types';
  import { grants } from '../change/presence.svelte';
  import { focus } from '../stores/focus.svelte';
  import { clock as hhmm, timeAgo } from '../change/format';
  import Icon from './Icon.svelte';

  let { presence }: { presence: Presence } = $props();

  $effect(() => {
    void presence.actor;
    void grants.load();
  });

  const grant = $derived(grants.of(presence.actor));
  const who = $derived(presence.actsFor ? `${presence.actsFor}/agent` : presence.actor);
  const host = $derived(window.location.hostname || '127.0.0.1');
</script>

<div class="banner" role="status">
  <Icon name="plug" size={18} />
  <span><b>{who}</b> <span class="via">via MCP on {host}</span></span>
  <span class="what mono" title={presence.text}>{presence.text ?? presence.tool ?? ''}</span>
  <span class="spacer"></span>
  <span class="muted small">
    {timeAgo(presence.at)}
    {#if grant?.expiresAt}· token approved {grant.approvedBy ? `by ${grant.approvedBy}` : 'today'}, ends {hhmm(grant.expiresAt)}{/if}
  </span>
  <label class="follow small" title="Follow the focus when an agent moves it">
    <input type="checkbox" bind:checked={focus.follow} /> follow
  </label>
</div>

<style>
  .banner {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    background: var(--info-soft);
    color: var(--ink);
    border-radius: var(--radius-s);
    padding: 8px 14px;
    font-size: 13.5px;
  }
  .banner :global(svg) {
    color: var(--info);
  }
  .via {
    color: var(--info);
  }
  .what {
    color: var(--muted);
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 40ch;
  }
  .follow {
    display: inline-flex;
    gap: 4px;
    align-items: center;
    color: var(--muted);
  }
</style>
