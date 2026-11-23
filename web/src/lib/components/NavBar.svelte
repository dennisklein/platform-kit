<script lang="ts">
  // Left icon nav (dark), as in the deck.
  import { router, href, seg, type NavKey } from '../router.svelte';
  import { focus } from '../stores/focus.svelte';
  import Icon from './Icon.svelte';

  const filesPath = $derived(
    focus.current?.changeId ? `/changes/${seg(focus.current.changeId)}/files` : '/files',
  );

  const items = $derived<{ key: NavKey; label: string; to: string }[]>([
    { key: 'changes', label: 'Changes', to: '/changes' },
    { key: 'files', label: 'Files', to: filesPath },
    { key: 'assets', label: 'Assets', to: '/assets' },
    { key: 'kb', label: 'Knowledge', to: '/kb' },
    { key: 'work', label: 'Work', to: '/work' },
    { key: 'labs', label: 'Labs', to: '/labs' },
    { key: 'chat', label: 'Chat', to: '/chat' },
    { key: 'history', label: 'History', to: '/history' },
  ]);
  const bottom: { key: NavKey; label: string; to: string }[] = [
    { key: 'clusters', label: 'Clusters', to: '/clusters' },
    { key: 'llm-log', label: 'LLM log', to: '/llm-log' },
    { key: 'settings', label: 'Settings', to: '/settings' },
  ];
  const active = $derived(router.route?.nav);
</script>

<nav class="nav" aria-label="Main">
  {#each items as it (it.key)}
    <a href={href(it.to)} class:active={active === it.key} aria-current={active === it.key ? 'page' : undefined}>
      <Icon name={it.key} size={20} />
      <span>{it.label}</span>
    </a>
  {/each}
  <span class="spacer"></span>
  {#each bottom as it (it.key)}
    <a href={href(it.to)} class:active={active === it.key} aria-current={active === it.key ? 'page' : undefined}>
      <Icon name={it.key} size={18} />
      <span>{it.label}</span>
    </a>
  {/each}
</nav>

<style>
  .nav {
    width: var(--nav-w);
    background: var(--nav-bg);
    display: flex;
    flex-direction: column;
    align-items: stretch;
    padding: 8px 0;
    gap: 2px;
    overflow-y: auto;
  }
  a {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    padding: 7px 2px;
    color: var(--nav-ink);
    text-decoration: none;
    font-size: 10.5px;
    border-left: 3px solid transparent;
    border-right: 3px solid transparent;
  }
  a:hover {
    color: #fff;
  }
  a.active {
    color: var(--nav-active);
    border-left-color: var(--nav-active);
    background: rgb(255 255 255 / 0.04);
  }
</style>
