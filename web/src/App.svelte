<script lang="ts">
  // App shell: top bar, dark icon nav, change rail, page, side panel.
  import { onMount } from 'svelte';
  import { fly } from 'svelte/transition';
  import { cubicOut } from 'svelte/easing';
  import { ms } from './lib/f/motion';
  import { routes } from './routes';
  import { router } from './lib/router.svelte';
  import { events } from './lib/stores/events.svelte';
  import { session } from './lib/stores/me.svelte';
  import { focus } from './lib/stores/focus.svelte';
  import { changes } from './lib/stores/changes.svelte';
  import { ui } from './lib/stores/ui.svelte';
  import TopBar from './lib/components/TopBar.svelte';
  import NavBar from './lib/components/NavBar.svelte';
  import ChangeRail from './lib/components/ChangeRail.svelte';
  import SidePanel from './lib/components/SidePanel.svelte';
  import NotFound from './routes/NotFound.svelte';
  import Toasts from './lib/components/Toasts.svelte';
  import NewChangeDialog from './routes/changes/NewChangeDialog.svelte';
  import FollowCue from './lib/components/FollowCue.svelte';

  onMount(() => {
    ui.applyTheme();
    router.start(routes);
    session.start();
    events.start();
    focus.start();
    changes.start();
    return () => events.stop();
  });

  // Report the page to the bench focus.
  $effect(() => {
    const r = router.route;
    if (!r) return;
    const line = Number(router.query.get('line'));
    focus.set({
      route: router.current,
      view: r.view,
      changeId: router.params.id,
      path: router.query.get('path') ?? undefined,
      line: Number.isFinite(line) && line > 0 ? line : undefined,
    });
  });

  $effect(() => {
    const t = router.route?.title;
    document.title = t ? `${t} · platform-kit bench` : 'platform-kit bench';
  });

  const Page = $derived(router.route?.component ?? NotFound);
</script>

<div class="shell" class:side={ui.sideOpen}>
  <TopBar />
  <div class="body">
    <NavBar />
    {#if router.route?.rail}
      <div class="rail-col">
        <ChangeRail onNew={() => (ui.newChangeOpen = true)} />
      </div>
    {/if}
    <main class="main">
      {#key router.route?.name}
        <Page params={router.params} query={router.query} />
      {/key}
    </main>
    {#if ui.sideOpen}
      <div class="side-col" in:fly={{ x: 24, duration: ms(200), easing: cubicOut }}>
        <SidePanel />
      </div>
    {/if}
  </div>
</div>

<NewChangeDialog bind:open={ui.newChangeOpen} />
<Toasts />
<FollowCue />

<style>
  .shell {
    height: 100%;
    display: grid;
    grid-template-rows: var(--topbar-h) minmax(0, 1fr);
  }
  .body {
    display: flex;
    min-height: 0;
    min-width: 0;
  }
  .rail-col {
    position: relative;
    width: var(--rail-w);
    flex: none;
    background: var(--bg);
    border-right: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
  .main {
    position: relative;
    flex: 1;
    min-width: 0;
    overflow: auto;
  }
  .side-col {
    display: flex;
    flex: none;
    min-height: 0;
  }
  /* Half a projector (browser next to a terminal): the side panel lies over
     the page instead of squeezing it. This is the only overlay rule; the
     nav and a strip of the page stay visible beside it. */
  @media (max-width: 1100px) {
    .side-col {
      position: fixed;
      top: var(--topbar-h);
      right: 0;
      bottom: 0;
      left: auto;
      z-index: 40;
      width: min(var(--side-w), calc(100vw - var(--nav-w) - 200px));
      box-shadow: -12px 0 32px rgb(0 0 0 / 0.18);
    }
    .side-col > :global(.side) {
      width: 100%;
    }
  }
  /* Projector widths (1280 px at 125 %): a narrower rail. */
  @media (max-width: 1100px) {
    .rail-col {
      width: 290px;
    }
  }
  /* The side panel takes the rail's room below 1440 px, so the page keeps
     its width; the Changes item of the nav bar still lists the changes. */
  @media (max-width: 1440px) {
    .shell.side .rail-col {
      display: none;
    }
  }
  @media (max-width: 820px) {
    .rail-col {
      display: none;
    }
  }
</style>
