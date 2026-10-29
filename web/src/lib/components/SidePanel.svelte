<script lang="ts">
  // Right side panel: chat (F) and live activity: the agents at work and a
  // transcript of their MCP calls (raw events only with ?debug). Below
  // 1100 px it floats over the page instead of squeezing it.
  import { ui } from '../stores/ui.svelte';
  import { focus } from '../stores/focus.svelte';
  import { events } from '../stores/events.svelte';
  import { mcpCalls } from '../stores/mcpcalls.svelte';
  import { router } from '../router.svelte';
  import { clock, timeAgo } from '../change/format';
  import { agentName, callArgs, reasonText, toCall } from '../f/mcp';
  import ChatPanel from '../../routes/chat/ChatPanel.svelte';
  import Icon from './Icon.svelte';
  import Glyph from './Glyph.svelte';

  const presence = $derived(
    Object.values(focus.presence)
      .filter((p) => p.kind !== 'human')
      .sort((a, b) => b.at.localeCompare(a.at)),
  );
  // The chat page shows the chat already: the panel shows the activity.
  const onChatPage = $derived(router.route?.name === 'chat');
  const tab = $derived(onChatPage ? 'activity' : ui.sideTab);
  const debug = $derived(router.query.has('debug') || new URLSearchParams(window.location.search).has('debug'));
</script>

<aside class="side" aria-label="Side panel">
  <div class="tabs" role="tablist">
    <button
      role="tab"
      aria-selected={tab === 'chat'}
      disabled={onChatPage}
      title={onChatPage ? 'The chat is open on this page' : undefined}
      onclick={() => ui.openSide('chat')}>Chat</button
    >
    <button role="tab" aria-selected={tab === 'activity'} onclick={() => ui.openSide('activity')}>
      Activity{#if mcpCalls.list.length}<span class="count">{mcpCalls.list.length}</span>{/if}
    </button>
    <span class="spacer"></span>
    <button class="btn ghost small" onclick={() => ui.toggleSide()} title="Close">
      <Icon name="close" size={14} /><span class="sr-only">Close</span>
    </button>
  </div>

  {#if focus.suggestion}
    <div class="suggest">
      <strong>{agentName(focus.suggestion)}</strong> suggests:
      {focus.suggestion.text ?? focus.suggestion.suggest?.route}
      <div class="row">
        <button class="btn small primary" onclick={() => focus.acceptSuggestion()}>Look</button>
        <button class="btn small" onclick={() => focus.dismissSuggestion()}>Dismiss</button>
      </div>
    </div>
  {/if}

  <div class="body">
    {#if tab === 'chat'}
      <ChatPanel />
    {:else}
      <h3 class="eyebrow">Agents</h3>
      {#each presence as p (p.actor)}
        {@const c = toCall(p)}
        <div class="agent">
          <span class="ico"><Glyph name="bot" size={15} /></span>
          <div>
            <div><strong>{agentName(p)}</strong> <span class="muted small">{timeAgo(p.at)}</span></div>
            <div class="mono small muted">{c.text}</div>
          </div>
        </div>
      {:else}
        <p class="muted small">No agent has called a tool in this tab yet.</p>
      {/each}

      {#if mcpCalls.list.length}
        <h3 class="eyebrow">MCP calls</h3>
        <ol class="calls">
          {#each mcpCalls.list as c, i (i)}
            <li class:denied={c.denied} class:failed={c.failed}>
              <span class="t mono" title={c.at}>{clock(c.at)}</span>
              <div class="call">
                <div>
                  <span class="tool mono">{c.tool}</span>
                  <span class="args mono">{callArgs(c)}</span>
                </div>
                {#if c.failed}
                  <div class="why">{reasonText(c.reason) || 'Failed'}</div>
                {/if}
              </div>
              <span class="res">
                {#if c.denied}<Glyph name="lock" size={12} />denied{:else if c.failed}<Glyph name="x" size={12} />failed{:else}<Glyph
                    name="check"
                    size={12}
                  />ok{/if}
              </span>
            </li>
          {/each}
        </ol>
      {/if}

      {#if debug}
        <h3 class="eyebrow">Events</h3>
        {#each events.recent as ev (ev.id)}
          <div class="ev small"><span class="mono muted">#{ev.id}</span> {ev.type}</div>
        {:else}
          <p class="muted small">No events yet.</p>
        {/each}
      {/if}
    {/if}
  </div>
</aside>

<style>
  .side {
    position: relative;
    width: var(--side-w);
    max-width: 100vw;
    background: var(--panel);
    border-left: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
  /* Narrow windows (a browser beside the agent terminal): float over the
     page, so the page keeps its width. */
  @media (max-width: 1100px) {
    .side {
      position: fixed;
      top: var(--topbar-h);
      right: 0;
      bottom: 0;
      width: min(420px, 90vw);
      z-index: 40;
      box-shadow: -8px 0 28px rgb(0 0 0 / 0.18);
    }
  }
  .tabs {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 6px 8px;
    border-bottom: 1px solid var(--line);
  }
  [role='tab'] {
    border: 0;
    background: none;
    padding: 4px 10px;
    border-radius: 5px;
    color: var(--muted);
    font-weight: 600;
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  [role='tab'][aria-selected='true'] {
    background: var(--accent-soft);
    color: var(--accent);
  }
  [role='tab']:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .count {
    font: 600 11px/1.5 var(--mono);
    background: var(--ai-soft);
    color: var(--ai);
    border-radius: 999px;
    padding: 0 6px;
  }
  .suggest {
    margin: 8px;
    padding: 8px 10px;
    border-radius: var(--radius-s);
    background: var(--ai-soft);
    border: 1px solid var(--ai-line);
    display: grid;
    gap: 6px;
    font-size: 13px;
  }
  .body {
    position: relative;
    flex: 1;
    overflow-y: auto;
    padding: 10px 12px;
    display: grid;
    align-content: start;
    gap: 8px;
    min-height: 0;
  }
  .body h3 {
    margin: 8px 0 0;
  }
  .agent {
    display: flex;
    gap: 8px;
    align-items: flex-start;
    font-size: 14px;
  }
  .ico {
    display: inline-grid;
    place-items: center;
    width: 26px;
    height: 26px;
    border-radius: 50%;
    background: var(--ai-soft);
    color: var(--ai);
    flex: none;
  }
  .calls {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 2px;
  }
  .calls li {
    display: grid;
    grid-template-columns: 40px minmax(0, 1fr) auto;
    gap: 8px;
    align-items: baseline;
    padding: 5px 6px;
    border-radius: 4px;
    font-size: 13px;
  }
  .calls li.denied {
    background: var(--bad-soft);
  }
  .t {
    color: var(--muted);
    font-size: 11.5px;
  }
  .call {
    min-width: 0;
  }
  .tool {
    font-weight: 600;
  }
  .args {
    color: var(--muted);
    overflow-wrap: anywhere;
  }
  .why {
    color: var(--bad);
    font-size: 12.5px;
    margin-top: 2px;
  }
  .failed:not(.denied) .why {
    color: var(--warn);
  }
  .res {
    display: inline-flex;
    gap: 3px;
    align-items: center;
    font: 600 11.5px var(--sans);
    color: var(--ok);
    white-space: nowrap;
  }
  .denied .res {
    color: var(--bad);
  }
  .failed:not(.denied) .res {
    color: var(--warn);
  }
  p {
    margin: 0;
  }
</style>
