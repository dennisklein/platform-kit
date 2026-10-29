<script lang="ts">
  // Commit the saved files of the worktree. "Suggest message" asks the
  // advisor (it reads the staged diff and the intent and writes nothing);
  // the admin edits the message and commits.
  import { tick } from 'svelte';
  import { api, must, errorText } from '../../lib/api/client';
  import type { AdvisorResult as Result, Change, CommitMessageSuggestion, Suggestion } from '../../lib/api/types';
  import Icon from '../../lib/components/Icon.svelte';
  import AdvisorResult from '../../lib/components/advisor/AdvisorResult.svelte';
  import AdvisorWorking from '../../lib/components/advisor/AdvisorWorking.svelte';
  import { RECIPE_TOOLS, minWorkingMs, readingSteps, reducedMotion, typedText } from '../../lib/f/advisor';
  import { toasts } from '../../lib/stores/toasts.svelte';
  import { change as changeStore } from '../../lib/stores/change.svelte';
  import { changes } from '../../lib/stores/changes.svelte';
  import { actions } from '../../lib/change/actions.svelte';
  import { href } from '../../lib/router.svelte';
  import { shortSha } from '../../lib/change/format';
  import { modelLabel } from '../../lib/f/llm';
  import { session } from '../../lib/stores/me.svelte';
  import StagedDiff from './StagedDiff.svelte';

  let {
    change,
    paths,
    oncommitted,
  }: { change: Change; paths: string[]; oncommitted: () => void } = $props();

  let message = $state('');
  let suggested = $state<CommitMessageSuggestion | null>(null);
  // The model did not answer when the message was suggested (fallback text).
  let suggestedOffline = $state(false);
  let edited = $state(false);
  let suggesting = $state(false);
  let committing = $state(false);
  let error = $state<string | null>(null);
  let last = $state<{ sha: string; subject: string } | null>(null);
  let check = $state<Result | null>(null);
  let checkKey = $state(0);
  let checking = $state(false);
  // The suggested message is being typed into the box.
  let typing = $state(false);
  let checkEl = $state<HTMLElement | null>(null);
  // An accepted suggestion of "Check this commit": its file is staged now,
  // so the panel shows the commit form; one line keeps the outcome in view.
  let accepted = $state<{ title: string; file: string } | null>(null);
  let acceptedEl = $state<HTMLElement | null>(null);
  let suggestCtl: AbortController | null = null;

  const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));
  const SUGGEST_TOOLS = RECIPE_TOOLS['commit-message'];
  const CHECK_TOOLS = RECIPE_TOOLS['check-commit'];

  /** Type the suggested message into the box (at once with reduced motion). */
  async function typeIn(text: string) {
    if (reducedMotion()) {
      message = text;
      return;
    }
    typing = true;
    const start = performance.now();
    await new Promise<void>((done) => {
      const step = () => {
        message = typedText(text, performance.now() - start);
        if (message.length >= text.length) done();
        else requestAnimationFrame(step);
      };
      requestAnimationFrame(step);
    });
    typing = false;
  }

  async function showCheck() {
    await tick();
    checkEl?.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'start' });
  }

  // The newest commit of the change (not the metadata one), for "Check this
  // commit" when the page was opened after committing.
  const newest = $derived.by(() => {
    const c = change.commits.find((x) => !/^(chore\(change\)|docs\(lab\)):/.test(x.subject));
    return c ? { sha: c.sha, subject: c.subject } : null;
  });
  const shown = $derived(last ?? (paths.length ? null : newest));
  const subject = $derived(message.split('\n')[0]?.trim() ?? '');
  const conventional = $derived(/^(feat|fix|chore|docs|refactor|test|ci|build|perf|revert|style)(\([\w./-]+\))?!?: \S/.test(subject));
  const canCommit = $derived(paths.length > 0 && subject.length > 0 && !committing);

  async function suggest() {
    suggestCtl?.abort();
    const ctl = new AbortController();
    suggestCtl = ctl;
    suggesting = true;
    error = null;
    message = '';
    suggested = null;
    try {
      const [s] = await Promise.all([
        must(api.POST('/changes/{id}/commit-message', { params: { path: { id: change.id } }, body: { paths }, signal: ctl.signal })),
        sleep(minWorkingMs(readingSteps(SUGGEST_TOOLS).length, reducedMotion())),
      ]);
      if (ctl.signal.aborted) return;
      suggested = s;
      suggestedOffline = !!session.status?.llm && !session.status.llm.mock && !session.status.llm.reachable;
      session.noteAnswer(s);
      suggesting = false;
      await typeIn(s.message);
      edited = false;
    } catch (e) {
      if (!ctl.signal.aborted) error = errorText(e);
    } finally {
      if (suggestCtl === ctl) {
        suggesting = false;
        suggestCtl = null;
      }
    }
  }

  function cancelSuggest() {
    suggestCtl?.abort();
    suggestCtl = null;
    suggesting = false;
  }

  async function onDecided(sg: Suggestion) {
    if (sg.status !== 'accepted') return;
    accepted = { title: sg.title || 'the suggested edit', file: sg.path.split('/').pop() ?? sg.path };
    await tick();
    acceptedEl?.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'nearest' });
  }

  async function commit() {
    if (!canCommit) return;
    committing = true;
    error = null;
    try {
      const c = await must(
        api.POST('/changes/{id}/commits', { params: { path: { id: change.id } }, body: { message: message.trim(), paths } }),
      );
      last = { sha: c.sha, subject: c.subject };
      toasts.ok(`Committed ${shortSha(c.sha)}`, c.subject);
      message = '';
      suggested = null;
      check = null;
      accepted = null;
      changeStore.later('commit', 50);
      changes.later(50);
      oncommitted();
    } catch (e) {
      error = errorText(e);
    } finally {
      committing = false;
    }
  }

  async function checkCommit() {
    checking = true;
    check = null;
    accepted = null;
    void showCheck();
    try {
      const [r] = await Promise.all([
        must(api.POST('/changes/{id}/advisors/{recipe}', { params: { path: { id: change.id, recipe: 'check-commit' } } })),
        sleep(minWorkingMs(readingSteps(CHECK_TOOLS).length, reducedMotion())),
      ]);
      check = r;
      checkKey += 1;
      void showCheck();
    } catch (e) {
      toasts.error('Advisor failed', e);
    } finally {
      checking = false;
    }
  }

  function onkey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      void commit();
    }
  }
</script>

<div class="commit">
  {#if accepted}
    <p class="accepted" bind:this={acceptedEl} role="status">
      <Icon name="check" size={15} />
      <span><strong>Accepted:</strong> {accepted.title} in <span class="mono">{accepted.file}</span>, now staged below.</span>
    </p>
  {/if}
  {#if paths.length === 0 && !message}
    {#if !shown}
      <!-- The worktree column says "Nothing to commit"; this says how. -->
      <section class="card nothing">
        <h3 class="eyebrow">Commit message</h3>
        <p class="muted">Save an edit in Files, then tick it on the left to commit it here.</p>
      </section>
    {/if}
  {:else}
    <section class="card">
      <h3 class="eyebrow">Commit message</h3>
      {#if suggesting}
        <AdvisorWorking title="Suggest a commit message" tools={SUGGEST_TOOLS} writer="Writing the message" onCancel={cancelSuggest} />
      {/if}
      <div class="msg" class:sug={suggested && !edited} hidden={suggesting}>
        {#if suggested}
          <div class="sugline">
            <Icon name="sparkle" size={15} />
            {typing ? 'suggested · typing…' : edited ? 'suggested, edited by you' : 'suggested · edit before you commit'}
            {#if suggested.mock}<span class="tag">built-in</span>{/if}
            {#if suggested.model !== 'mock'}
              {@const ml = modelLabel(suggested, suggestedOffline)}
              <span class="mono small model" class:fb={ml.fallback} title={ml.title}>{ml.text}</span>
            {/if}
          </div>
        {/if}
        <textarea
          class="mono"
          rows="7"
          bind:value={message}
          oninput={() => (edited = true)}
          onkeydown={onkey}
          readonly={typing || suggesting}
          placeholder={'Write a Conventional Commit message: type(scope): what changes.\nOr click Suggest message.'}
          aria-label="Commit message"
        ></textarea>
      </div>
      {#if subject && !conventional && !typing}
        <p class="hint warn-text">Use a Conventional Commit subject, for example <code>feat(inventory): …</code>. The CI checks it.</p>
      {/if}
      {#if error}<div class="error-box">{error}</div>{/if}
      <div class="row">
        <button class="btn ai" onclick={suggest} disabled={suggesting || typing || paths.length === 0}>
          <Icon name="sparkle" size={15} />{suggested ? 'Suggest again' : 'Suggest message'}
        </button>
        <button class="btn dark" onclick={commit} disabled={!canCommit || typing}>
          <Icon name="check" size={15} />{committing ? 'Committing…' : `Commit ${paths.length} file${paths.length === 1 ? '' : 's'}`}
        </button>
        <span class="muted small kbd">Ctrl+Enter</span>
      </div>
    </section>

    <div class="callout llm">
      <Icon name="sparkle" size={18} />
      <span>The advisor reads the staged diff and the change intent. It writes nothing: the admin edits and commits.</span>
    </div>
  {/if}

  {#if paths.length}
    <StagedDiff changeId={change.id} head={change.headSha} {paths} />
  {/if}

  {#if shown}
    <section class="card done">
      <div class="row">
        <Icon name="check" size={16} />
        <span>{last ? '' : 'Last commit '}<span class="mono accent">{shortSha(shown.sha)}</span> {shown.subject}</span>
      </div>
      <div class="row">
        <button class="btn ai" class:on={checking || !!check} aria-pressed={checking || !!check} onclick={checkCommit} disabled={checking}>
          <Icon name="sparkle" size={15} />{checking ? 'Checking…' : 'Check this commit'}
        </button>
        <button class="btn" onclick={() => actions.runLab(change.id)} disabled={actions.isBusy(change.id)}><Icon name="flask" size={15} />Run lab</button>
        <button class="btn" onclick={() => actions.push(change.id)} disabled={actions.isBusy(change.id)}>
          <Icon name="upload" size={15} />{change.mr ? 'Push' : 'Push, open draft MR'}
        </button>
        <a class="btn" href={href(`/changes/${change.id}`)}>Back to change</a>
      </div>
      <div class="checkres" bind:this={checkEl}>
        {#if checking}
          <AdvisorWorking title="Check this commit" tools={CHECK_TOOLS} />
        {:else if check}
          {#key checkKey}
            <AdvisorResult result={check} reveal onRerun={checkCommit} busy={checking} {onDecided} />
          {/key}
        {/if}
      </div>
    </section>
  {/if}
</div>

<style>
  .commit {
    display: grid;
    gap: 16px;
    padding: 20px 24px;
    max-width: 1000px;
  }
  .card {
    display: grid;
    gap: 12px;
  }
  .msg {
    border: 1px solid var(--line);
    border-radius: 6px;
    background: var(--bg);
    display: grid;
  }
  .msg:focus-within {
    border-color: var(--ink);
  }
  .msg.sug {
    border-color: var(--ai-line);
    box-shadow: inset 3px 0 0 var(--ai-fill);
  }
  .sugline {
    display: flex;
    gap: 6px;
    align-items: center;
    padding: 10px 14px 0;
    color: var(--ai);
    font-weight: 600;
    font-size: 14px;
    min-width: 0;
  }
  .msg[hidden] {
    display: none;
  }
  .accepted {
    margin: 0;
    display: flex;
    gap: 8px;
    align-items: flex-start;
    padding: 10px 14px;
    border: 1px solid var(--ai-line);
    border-left: 4px solid var(--ok);
    border-radius: var(--radius);
    background: var(--panel);
    font-size: 14.5px;
    scroll-margin-top: 16px;
  }
  .accepted :global(svg) {
    color: var(--ok);
    flex: none;
    margin-top: 2px;
  }
  .sugline .model {
    color: var(--muted);
    font-weight: 400;
  }
  .card.nothing p {
    margin: 0;
  }
  .btn.ai :global(svg) {
    color: var(--ai);
  }
  .btn.ai:hover:not(:disabled) {
    border-color: var(--ai-line);
    background: var(--ai-soft);
  }
  .btn.ai.on,
  .btn.ai.on:hover:not(:disabled),
  .btn.ai.on:disabled {
    background: var(--ai-fill);
    border-color: var(--ai-fill);
    color: var(--ai-on);
    font-weight: 600;
    opacity: 1;
  }
  .btn.ai.on :global(svg) {
    color: var(--ai-on);
  }
  .checkres {
    scroll-margin-top: 16px;
    min-width: 0;
  }
  .checkres:empty {
    display: none;
  }
  .tag {
    font: 600 11px var(--sans);
    background: var(--panel);
    color: var(--muted);
    border-radius: 4px;
    padding: 1px 6px;
  }
  textarea {
    border: 0;
    background: transparent;
    color: var(--ink);
    padding: 10px 14px 14px;
    font-size: 14px;
    line-height: 1.55;
    resize: vertical;
    outline: none;
    min-height: 140px;
  }
  .row {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .kbd {
    margin-left: 4px;
  }
  .hint {
    margin: 0;
    font-size: 13px;
  }
  .callout.llm {
    display: flex;
    gap: 12px;
    align-items: center;
    border-left: 4px solid var(--ai-fill);
    background: var(--ai-soft);
    padding: 12px 16px;
    color: var(--ink);
  }
  .callout.llm :global(svg) {
    color: var(--ai);
    flex: none;
  }
  .done > .row:first-child :global(svg) {
    color: var(--ok);
  }
  .accent {
    color: var(--accent);
  }
</style>
