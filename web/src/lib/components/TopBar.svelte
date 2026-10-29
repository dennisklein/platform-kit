<script lang="ts">
  import { session } from '../stores/me.svelte';
  import { focus } from '../stores/focus.svelte';
  import { changes } from '../stores/changes.svelte';
  import { events } from '../stores/events.svelte';
  import { ui } from '../stores/ui.svelte';
  import { href, seg } from '../router.svelte';
  import LifecycleChip from '../change/LifecycleChip.svelte';
  import Icon from './Icon.svelte';
  import Glyph from './Glyph.svelte';
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
  const flux = $derived({ tone: pillClass(st?.flux.reachable), label: st?.flux.name ?? 'Flux', title: st?.flux.detail ?? '' });
  const live = $derived({
    tone: events.connected ? 'ok' : 'warn',
    title: events.connected ? 'Live updates on' : 'Live updates off, reconnecting',
  });
  const dark = $derived(ui.shownTheme === 'dark');
</script>

<header class="topbar">
  <a class="brand" href={href('/changes')} title="platform-kit bench">
    <span class="mark" aria-hidden="true">
      <svg viewBox="0 0 24 24" width="24" height="24" fill="none" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round">
        <path d="M12 3 3 7.5l9 4.5 9-4.5z" fill="var(--gsi-orange)" stroke="var(--gsi-orange)" />
        <path d="M3 12l9 4.5 9-4.5" stroke="#fff" />
        <path d="M3 16.5 12 21l9-4.5" stroke="#fff" opacity=".55" />
      </svg>
    </span>
    <span class="words">
      <span class="name">platform-kit</span>
      <span class="sub">bench · {st?.mode === 'shared' ? 'shared' : 'local'}</span>
    </span>
  </a>

  {#if current}
    <a class="change" href={href(`/changes/${seg(current.id)}`)} title="{current.id}: {current.title}">
      <Glyph name="branch" size={15} />
      <span class="cid">{current.id}</span>
      <span class="ctitle">{current.title}</span>
      <LifecycleChip change={current} />
    </a>
  {/if}

  <span class="spacer"></span>

  <!-- Below 900 px (browser and terminal side by side) the pills keep their
       dot and tooltip and drop the words. -->
  <div class="pills">
    <span class="pill {forge.tone}" title={forge.title} aria-label={forge.label}>
      <span class="dot"></span><span class="pl">{forge.label}</span>
    </span>
    <span class="pill ai {llm.tone}" title={llm.title} aria-label={llm.label}>
      <span class="dot"></span><Glyph name="sparkle" size={12} /><span class="pl">{llm.label}</span>
    </span>
    <a class="pill {flux.tone}" href={href('/clusters')} title={flux.title} aria-label={flux.label}>
      <span class="dot"></span><span class="pl">{flux.label}</span>
    </a>
    <span class="pill {live.tone}" title={live.title} aria-label="live">
      <span class="dot"></span><span class="pl">live</span>
    </span>
  </div>

  <button
    class="btn ghost icon"
    onclick={() => ui.toggleTheme()}
    title={dark ? 'Switch to the light theme' : 'Switch to the dark theme'}
  >
    <Icon name={dark ? 'moon' : 'sun'} size={18} />
    <span class="sr-only">{dark ? 'Light theme' : 'Dark theme'}</span>
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
    gap: 12px;
    padding: 0 12px 0 0;
    background: var(--panel);
    border-bottom: 1px solid var(--line);
    min-width: 0;
  }
  /* The mark sits in a navy cell as wide as the nav bar, as in the deck. */
  .brand {
    display: inline-flex;
    align-items: center;
    color: var(--ink);
    text-decoration: none;
    white-space: nowrap;
    min-width: 0;
    flex: none;
  }
  .mark {
    align-self: stretch;
    width: var(--nav-w);
    height: var(--topbar-h);
    margin-bottom: -1px;
    display: grid;
    place-items: center;
    background: var(--nav-bg);
    flex: none;
    margin-right: 14px;
  }
  .words {
    display: inline-flex;
    align-items: baseline;
    gap: 8px;
  }
  .name {
    font-weight: 700;
    font-size: 15px;
    letter-spacing: -0.01em;
  }
  .sub {
    font-size: 12.5px;
    color: var(--muted);
  }
  /* Current change: branch icon, id, the full title, state. The title gives
     way only when the bar runs out of room. */
  .change {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    height: 30px;
    padding: 0 6px 0 10px;
    margin-left: 6px;
    border: 1px solid var(--line-strong);
    border-radius: 6px;
    background: var(--panel);
    color: var(--ink);
    text-decoration: none;
    font-size: 13px;
    white-space: nowrap;
    min-width: 0;
    max-width: min(76ch, 50vw);
    flex: 0 1 auto;
  }
  .change:hover {
    border-color: var(--accent-fill);
  }
  .change :global(svg) {
    color: var(--accent);
    flex: none;
  }
  .cid {
    font: 12.5px var(--mono);
    color: var(--muted);
    flex: none;
  }
  .ctitle {
    min-width: 0;
    flex: 0 1 auto;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .change :global(.schip) {
    flex: none;
  }
  .pills {
    display: flex;
    gap: 6px;
    flex-wrap: nowrap;
    flex: none;
  }
  .pills a {
    text-decoration: none;
  }
  /* LLM pill: the AI teal, with the dot for its health. */
  .pill.ai {
    color: var(--ai);
    border-color: var(--ai-line);
  }
  .pill.ai :global(svg) {
    flex: none;
    margin-left: -2px;
  }
  .pill.ai.ok .dot {
    background: var(--ok);
  }
  .pill.ai.warn .dot {
    background: var(--warn);
  }
  .icon {
    padding: 4px 6px;
    color: var(--muted);
    flex: none;
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
  @media (max-width: 1180px) {
    .sub {
      display: none;
    }
  }
  /* Half a projector (about 768 px): keep the change chip and the status
     dots; the words go into the tooltips. */
  @media (max-width: 900px) {
    .topbar {
      gap: 8px;
    }
    .pl,
    .pill.ai :global(svg) {
      display: none;
    }
    .pill {
      padding: 6px;
      gap: 0;
    }
    .pills {
      gap: 4px;
    }
    .change {
      margin-left: 0;
    }
    .ctitle {
      max-width: 22ch;
    }
  }
  @media (max-width: 640px) {
    .name,
    .ctitle {
      display: none;
    }
    .mark {
      margin-right: 0;
    }
  }
  @media (max-width: 480px) {
    .pills {
      display: none;
    }
  }
</style>
