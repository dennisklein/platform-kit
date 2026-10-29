<script lang="ts">
  // Right side panel: chat (F) and live activity (presence, events).
  import { ui } from '../stores/ui.svelte';
  import { focus } from '../stores/focus.svelte';
  import { events } from '../stores/events.svelte';
  import ChatPanel from '../../routes/chat/ChatPanel.svelte';
  import Icon from './Icon.svelte';

  const presence = $derived(Object.values(focus.presence).sort((a, b) => b.at.localeCompare(a.at)));
</script>

<aside class="side" aria-label="Side panel">
  <div class="tabs" role="tablist">
    <button role="tab" aria-selected={ui.sideTab === 'chat'} onclick={() => ui.openSide('chat')}>Chat</button>
    <button role="tab" aria-selected={ui.sideTab === 'activity'} onclick={() => ui.openSide('activity')}>
      Activity
    </button>
    <span class="spacer"></span>
    <button class="btn ghost small" onclick={() => ui.toggleSide()} title="Close">
      <Icon name="close" size={14} /><span class="sr-only">Close</span>
    </button>
  </div>

  {#if focus.suggestion}
    <div class="suggest">
      <strong>{focus.suggestion.actor}</strong> suggests:
      {focus.suggestion.text ?? focus.suggestion.suggest?.route}
      <div class="row">
        <button class="btn small primary" onclick={() => focus.acceptSuggestion()}>Look</button>
        <button class="btn small" onclick={() => focus.dismissSuggestion()}>Dismiss</button>
      </div>
    </div>
  {/if}

  <div class="body">
    {#if ui.sideTab === 'chat'}
      <ChatPanel />
    {:else}
      <h3>Agents</h3>
      {#each presence as p (p.actor)}
        <div class="act">
          <Icon name="bot" size={14} />
          <span><strong>{p.actor}</strong> <span class="muted">{p.text ?? p.tool ?? ''}</span></span>
        </div>
      {:else}
        <p class="muted small">No agent activity.</p>
      {/each}
      <h3>Events</h3>
      {#each events.recent as ev (ev.id)}
        <div class="ev small"><span class="mono muted">#{ev.id}</span> {ev.type}</div>
      {:else}
        <p class="muted small">No events yet.</p>
      {/each}
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
  }
  [role='tab'][aria-selected='true'] {
    background: var(--accent-soft);
    color: var(--accent);
  }
  .suggest {
    margin: 8px;
    padding: 8px 10px;
    border-radius: var(--radius-s);
    background: var(--info-soft);
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
    gap: 6px;
    min-height: 0;
  }
  .act {
    display: flex;
    gap: 6px;
    align-items: baseline;
    font-size: 13px;
  }
  p {
    margin: 0;
  }
</style>
