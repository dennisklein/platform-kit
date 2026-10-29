<script lang="ts">
  // Banner on a change an agent works on: who, through what, the last call
  // (a red "denied" pill with the reason when the bench refused it), until
  // when, and whether this screen follows the agent.
  import { onMount } from 'svelte';
  import type { Presence } from '../api/types';
  import { grants } from '../change/presence.svelte';
  import { focus } from '../stores/focus.svelte';
  import { clock as hhmm, timeAgo } from '../change/format';
  import { agentName, reasonText, routeLabel, toCall } from '../f/mcp';
  import Icon from './Icon.svelte';
  import Glyph from './Glyph.svelte';

  let { presence }: { presence: Presence } = $props();

  $effect(() => {
    void presence.actor;
    void grants.load();
  });

  const grant = $derived(grants.of(presence.actor));
  const who = $derived(agentName(presence));
  const host = $derived(window.location.hostname || '127.0.0.1');
  const call = $derived(toCall(presence));

  // The banner stands in for the floating follow cue while it is on the page.
  onMount(() => {
    focus.banners += 1;
    return () => (focus.banners -= 1);
  });
  let now = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => (now = Date.now()), 500);
    return () => clearInterval(t);
  });
  const move = $derived(focus.lastMove);
  // The screen just moved with the agent: say where, for a few seconds.
  const moved = $derived(!!move && move.followed && focus.follow && now - move.at < 6000);
  const skipped = $derived(!!move && !move.followed && !focus.follow);
</script>

<div class="banner" class:denied={call.denied} class:failed={call.failed && !call.denied} role="status">
  <Icon name="plug" size={18} />
  <span><b>{who}</b> <span class="via">via MCP on {host}</span></span>
  <span class="what mono" title={presence.text}>{call.text}</span>
  {#if call.failed}
    <span class="pill">{call.denied ? 'denied' : 'failed'}</span>
    {#if call.reason && call.reason !== 'failed'}<span class="reason">{reasonText(call.reason)}</span>{/if}
  {/if}
  <span class="spacer"></span>
  <span class="muted small">
    {timeAgo(presence.at)}
    {#if grant?.expiresAt}· token approved {grant.approvedBy ? `by ${grant.approvedBy}` : 'today'}, ends {hhmm(grant.expiresAt)}{/if}
  </span>
  {#if skipped && move}
    <button class="follow small" onclick={() => focus.look()} title="Go where the agent is now">
      <Glyph name="eye" size={13} />Look: {routeLabel(move.route)}
    </button>
  {/if}
  <button
    class="follow small"
    class:on={focus.follow}
    class:moved
    aria-pressed={focus.follow}
    onclick={() => (focus.follow ? focus.pause() : focus.resume())}
    title={focus.follow ? 'This screen moves where the agent works. Click to pause.' : 'Paused. Click to follow the agent again.'}
  >
    {#if focus.follow}
      <Glyph name="eye" size={13} />{moved && move ? `Following → ${routeLabel(move.route)}` : 'Following'}
      <span class="hint">· Pause</span>
    {:else}
      <Glyph name="play" size={13} />Paused · Resume
    {/if}
  </button>
</div>

<style>
  .banner {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
    background: var(--ai-soft);
    color: var(--ink);
    border: 1px solid var(--ai-line);
    border-left: 4px solid var(--ai-fill);
    border-radius: var(--radius-s);
    padding: 8px 14px;
    font-size: 14.5px;
  }
  .banner > :global(svg) {
    color: var(--ai);
  }
  .via {
    color: var(--ai);
  }
  .what {
    color: var(--muted);
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 40ch;
  }
  .pill {
    font: 700 12px/1.6 var(--sans);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    border-radius: 4px;
    padding: 0 8px;
    background: var(--bad);
    color: #fff;
  }
  .reason {
    color: var(--bad);
    font-weight: 600;
  }
  .banner.denied {
    background: var(--bad-soft);
    border-color: color-mix(in srgb, var(--bad) 45%, transparent);
    border-left-color: var(--bad);
  }
  .banner.failed .pill {
    background: var(--warn);
  }
  .banner.failed .reason {
    color: var(--warn);
  }
  .follow {
    display: inline-flex;
    gap: 5px;
    align-items: center;
    border: 1px solid var(--ai-line);
    background: var(--panel);
    color: var(--muted);
    border-radius: 999px;
    padding: 2px 10px;
    font-weight: 600;
    cursor: pointer;
  }
  .follow.on {
    color: var(--ai);
  }
  .follow .hint {
    font-weight: 400;
    color: var(--muted);
  }
  /* The screen just moved with the agent. */
  .follow.moved {
    background: var(--ai-fill);
    border-color: var(--ai-fill);
    color: var(--ai-on);
    animation: moved 0.5s ease-out;
  }
  .follow.moved .hint {
    color: inherit;
    opacity: 0.8;
  }
  @keyframes moved {
    from {
      box-shadow: 0 0 0 6px color-mix(in srgb, var(--ai-fill) 35%, transparent);
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .follow.moved {
      animation: none;
    }
  }
</style>
