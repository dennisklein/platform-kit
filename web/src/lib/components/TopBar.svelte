<script lang="ts">
  import { session } from '../stores/me.svelte';
  import { focus } from '../stores/focus.svelte';
  import { changes } from '../stores/changes.svelte';
  import { events } from '../stores/events.svelte';
  import { ui } from '../stores/ui.svelte';
  import { href, seg } from '../router.svelte';
  import StateChip from './StateChip.svelte';
  import Icon from './Icon.svelte';
  import { llmPill } from '../f/llm';
  import { forgePill } from '../f/forge';

  const st = $derived(session.status);
  const current = $derived(changes.byId(focus.current?.changeId));
  const initials = $derived(
    session.me?.initials ||
      (session.me?.name ?? session.me?.id ?? '?')
        .split(/[\s.-]+/)
        .map((p) => p[0]?.toUpperCase() ?? '')
        .join('')
        .slice(0, 2),
  );
  // "LLM offline" when the model does not answer; the reason is in the tooltip.
  const llm = $derived(llmPill(st?.llm));
  // "Gitea offline" when the ping fails or the change list is the local state.
  const forge = $derived(forgePill(st?.forge, changes.forgeOffline));
  const pillClass = (ok: boolean | undefined) => (ok === undefined ? '' : ok ? 'ok' : 'warn');
</script>

<header class="topbar">
  <a class="brand" href={href('/changes')}>
    <span class="logo" aria-hidden="true">pk</span>
    <span class="btext">platform-kit · bench · {st?.mode === 'shared' ? 'shared' : 'local'} mode</span>
  </a>

  {#if current}
    <a class="pill current" href={href(`/changes/${seg(current.id)}`)} title={current.intent}>
      <span class="mono">{current.id}</span>
      <span class="ctitle">{current.title}</span>
      <StateChip state={current.state} labState={current.labState} />
    </a>
  {/if}

  <span class="spacer"></span>

  <div class="pills">
    <span class="pill {forge.tone}" title={forge.title}>
      <span class="dot"></span>{forge.label}
    </span>
    <span class="pill {llm.tone}" title={llm.title}>
      <span class="dot"></span>{llm.label}
    </span>
    <a class="pill {pillClass(st?.flux.reachable)}" href={href('/clusters')} title={st?.flux.detail}>
      <span class="dot"></span>{st?.flux.name ?? 'Flux'}
    </a>
    <span
      class="pill {events.connected ? 'ok' : 'warn'}"
      title={events.connected ? 'Live updates on' : 'Live updates off, reconnecting'}
    >
      <span class="dot"></span>live
    </span>
  </div>

  <button class="btn ghost icon" onclick={() => ui.cycleTheme()} title="Theme: {ui.theme}">
    <Icon name={ui.theme === 'dark' ? 'moon' : 'sun'} size={18} />
    <span class="sr-only">Theme</span>
  </button>
  <button
    class="btn ghost icon"
    onclick={() => ui.toggleSide()}
    aria-pressed={ui.sideOpen}
    title="Side panel"
  >
    <Icon name="panel" size={18} />
    <span class="sr-only">Side panel</span>
  </button>
  <a class="avatar" href={href('/settings')} title={session.me ? `${session.me.name} (${session.me.id})` : 'Not signed in'}>
    {initials}
  </a>
</header>

<style>
  .topbar {
    height: var(--topbar-h);
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 0 12px 0 16px;
    background: var(--panel);
    border-bottom: 1px solid var(--line);
    min-width: 0;
  }
  .brand {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-weight: 700;
    color: var(--ink);
    text-decoration: none;
    white-space: nowrap;
    min-width: 0;
  }
  .btext {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .logo {
    flex: none;
    display: inline-grid;
    place-items: center;
    width: 24px;
    height: 24px;
    border-radius: 5px;
    background: var(--gsi-navy);
    color: var(--gsi-orange);
    font: 700 12px/1 var(--mono);
  }
  .current {
    color: var(--ink);
    text-decoration: none;
    min-width: 0;
    max-width: 44ch;
  }
  .ctitle {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .pills {
    display: flex;
    gap: 6px;
    flex-wrap: nowrap;
  }
  .pills a {
    text-decoration: none;
  }
  .icon {
    padding: 4px 6px;
    color: var(--muted);
  }
  .icon[aria-pressed='true'] {
    color: var(--accent);
  }
  .avatar {
    display: inline-grid;
    place-items: center;
    width: 30px;
    height: 30px;
    border-radius: 50%;
    background: var(--gsi-orange);
    color: var(--navy-900);
    font-weight: 700;
    font-size: 12px;
    text-decoration: none;
    flex: none;
  }
  @media (max-width: 900px) {
    .pills,
    .current {
      display: none;
    }
  }
  /* Phone width: the logo stands for the name, so the avatar stays inside. */
  @media (max-width: 520px) {
    .btext {
      display: none;
    }
  }
</style>
