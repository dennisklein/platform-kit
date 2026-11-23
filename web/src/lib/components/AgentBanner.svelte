<script lang="ts">
  // Banner on a change an agent works on: who, through what, the last call
  // (a red "denied" pill with the reason when the bench refused it), until
  // when, and whether this screen follows the agent. It sticks to the top of
  // the page column. A denial turns into a calm "Agent stopped" line after
  // 15 s or when a person acts on the change.
  import { onMount } from 'svelte';
  import type { Presence } from '../api/types';
  import { grants } from '../change/presence.svelte';
  import { focus } from '../stores/focus.svelte';
  import { events } from '../stores/events.svelte';
  import { clock as hhmm, timeAgo } from '../change/format';
  import { agentName, deniedLeft, reasonText, routeLabel, stoppedText, toCall } from '../f/mcp';
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

  // The banner stands in for the floating follow cue (and the denial toast)
  // while it is in view.
  let el = $state<HTMLElement | null>(null);
  onMount(() => {
    const cid = presence.changeId;
    let seen = false;
    const set = (v: boolean) => {
      if (v === seen) return;
      seen = v;
      focus.bannerSeen(cid, v ? 1 : -1);
    };
    const io =
      typeof IntersectionObserver === 'undefined' ? null : new IntersectionObserver((es) => set(es.some((e) => e.isIntersecting)));
    if (io && el) io.observe(el);
    else set(true);
    // The height, for pages that scroll a card into view under the banner.
    const root = document.documentElement;
    const ro = typeof ResizeObserver === 'undefined' || !el ? null : new ResizeObserver(() => root.style.setProperty('--agent-banner-h', `${(el?.offsetHeight ?? 0) + 8}px`));
    if (ro && el) ro.observe(el);
    return () => {
      io?.disconnect();
      ro?.disconnect();
      root.style.removeProperty('--agent-banner-h');
      set(false);
    };
  });

  // Denied: red for 15 s, or until a person acts on the change; then calm.
  let calm = $state(false);
  $effect(() => {
    const c = call;
    calm = false;
    if (!c.denied) return;
    const left = deniedLeft(c.at, Date.now());
    if (left === 0) {
      calm = true;
      return;
    }
    const t = setTimeout(() => (calm = true), left);
    const cid = c.changeId;
    const since = Date.now();
    // Change and lab events after the denial come from people: the agent stopped.
    const human = (d: { changeId?: string } | undefined) => {
      if (d?.changeId === cid && Date.now() - since > 1000) calm = true;
    };
    const offs = [
      events.on('change.updated', human),
      events.on('lab.step', human),
      events.on<Presence>('presence', (p) => {
        if (p.kind === 'human' && p.changeId === cid) calm = true;
      }),
    ];
    return () => {
      clearTimeout(t);
      offs.forEach((off) => off());
    };
  });
  const denied = $derived(call.denied && !calm);
  const stopped = $derived(call.denied && calm);
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

<div class="banner" class:denied class:failed={call.failed && !call.denied} role="status" bind:this={el}>
  <Icon name="plug" size={18} />
  <span><b>{who}</b> <span class="via">via MCP on {host}</span></span>
  {#if stopped}
    <span class="stopped" title={`${call.text}: ${reasonText(call.reason)}`}>{stoppedText(call)}</span>
  {:else}
    <span class="what mono" title={presence.text}>{call.text}</span>
    {#if call.failed}
      <span class="pill">{call.denied ? 'denied' : 'failed'}</span>
      {#if call.reason && call.reason !== 'failed'}<span class="reason">{reasonText(call.reason)}</span>{/if}
    {/if}
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
    /* Stays in view when the page scrolls (the advisor jump of pk_advise). */
    position: sticky;
    top: 8px;
    z-index: 6;
    box-shadow: 0 4px 14px -8px rgb(0 0 0 / 0.35);
    transition:
      background 0.4s ease,
      border-color 0.4s ease;
  }
  .stopped {
    color: var(--muted);
    font-weight: 600;
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
    .banner {
      transition: none;
    }
  }
</style>
