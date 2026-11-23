<script lang="ts">
  // When an agent moves the screen (navigate mode), say so at the top:
  // "Following dennis/agent → review of c-k7m2qz", with Pause. While
  // following is paused, a small pill keeps Resume (and Look) at hand.
  // Pages with an agent banner show this in the banner instead.
  import { fly } from 'svelte/transition';
  import { focus } from '../stores/focus.svelte';
  import { routeLabel } from '../f/mcp';
  import { reducedMotion } from '../f/advisor';
  import Glyph from './Glyph.svelte';

  /** How long the cue stays after a move the screen followed. */
  const SHOW_MS = 6000;

  let now = $state(Date.now());
  $effect(() => {
    const t = setInterval(() => (now = Date.now()), 500);
    return () => clearInterval(t);
  });

  let dismissed = $state(0);
  const m = $derived(focus.lastMove);
  // An agent banner on the page says the same, without covering anything.
  const following = $derived(!!m && m.followed && focus.follow && now - m.at < SHOW_MS && focus.banners === 0);
  const paused = $derived(!focus.follow && dismissed < (m?.at ?? 1) && focus.banners === 0);
  const dur = reducedMotion() ? 0 : 220;
</script>

{#if following && m}
  <div class="cue" role="status" transition:fly={{ y: -14, duration: dur }}>
    <Glyph name="bot" size={18} />
    <span>Following <b>{m.by}</b> <Glyph name="arrow-right" size={14} /> {routeLabel(m.route)}</span>
    <button class="act" onclick={() => focus.pause()} title="Stop moving this screen with the agent">
      <Glyph name="pause" size={13} />Pause following
    </button>
  </div>
{:else if paused}
  <div class="cue paused" role="status" transition:fly={{ y: -14, duration: dur }}>
    <Glyph name="pause" size={15} />
    {#if m && !m.followed}
      <span>Paused. <b>{m.by}</b> is on {routeLabel(m.route)}</span>
      <button class="act" onclick={() => focus.look()}><Glyph name="eye" size={13} />Look</button>
    {:else}
      <span>Following agents is paused</span>
    {/if}
    <button class="act" onclick={() => focus.resume()}><Glyph name="play" size={13} />Resume</button>
    <button class="x" onclick={() => (dismissed = Date.now())} title="Hide until the agent moves again">
      <Glyph name="x" size={13} /><span class="sr-only">Hide</span>
    </button>
  </div>
{/if}

<style>
  .cue {
    position: fixed;
    top: calc(var(--topbar-h) + 10px);
    left: 50%;
    transform: translateX(-50%);
    z-index: 60;
    display: flex;
    align-items: center;
    gap: 10px;
    max-width: min(760px, calc(100vw - 32px));
    padding: 8px 10px 8px 16px;
    border-radius: 999px;
    background: var(--ai-fill);
    color: var(--ai-on);
    font-size: 15px;
    box-shadow: 0 6px 24px rgb(0 0 0 / 0.22);
  }
  .cue > span {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .cue.paused {
    background: var(--panel);
    color: var(--ink);
    border: 1px solid var(--ai-line);
    font-size: 14px;
  }
  .cue.paused > :global(svg) {
    color: var(--ai);
  }
  .act {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    border: 1px solid color-mix(in srgb, currentColor 45%, transparent);
    background: transparent;
    color: inherit;
    border-radius: 999px;
    padding: 3px 11px;
    font: 600 13px var(--sans);
    white-space: nowrap;
    cursor: pointer;
  }
  .act:hover {
    background: color-mix(in srgb, currentColor 14%, transparent);
  }
  .paused .act {
    color: var(--ai);
    border-color: var(--ai-line);
  }
  .x {
    border: 0;
    background: none;
    color: var(--muted);
    padding: 3px;
    display: inline-grid;
    place-items: center;
    cursor: pointer;
  }
</style>
