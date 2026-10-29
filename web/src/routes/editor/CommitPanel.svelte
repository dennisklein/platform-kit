<script lang="ts">
  // Commit the saved files of the worktree. "Suggest message" asks the
  // advisor (it reads the staged diff and the intent and writes nothing);
  // the admin edits the message and commits.
  import { api, must, errorText } from '../../lib/api/client';
  import type { AdvisorResult as Result, Change, CommitMessageSuggestion } from '../../lib/api/types';
  import Icon from '../../lib/components/Icon.svelte';
  import AdvisorResult from '../../lib/components/advisor/AdvisorResult.svelte';
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
  let checking = $state(false);

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
    suggesting = true;
    error = null;
    try {
      const s = await must(
        api.POST('/changes/{id}/commit-message', { params: { path: { id: change.id } }, body: { paths } }),
      );
      suggested = s;
      suggestedOffline = !!session.status?.llm && !session.status.llm.mock && !session.status.llm.reachable;
      session.noteAnswer(s);
      message = s.message;
      edited = false;
    } catch (e) {
      error = errorText(e);
    } finally {
      suggesting = false;
    }
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
    try {
      check = await must(
        api.POST('/changes/{id}/advisors/{recipe}', { params: { path: { id: change.id, recipe: 'check-commit' } } }),
      );
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
  <section class="card">
    <h3 class="eyebrow">Commit message</h3>
    <div class="msg" class:sug={suggested && !edited}>
      {#if suggested}
        <div class="sugline">
          <Icon name="sparkle" size={15} />
          {edited ? 'suggested, edited by you' : 'suggested · edit before you commit'}
          {#if suggested.mock}<span class="tag">mock LLM</span>{/if}
          {#if suggested.model !== 'mock'}
            {@const ml = modelLabel(suggested, suggestedOffline)}
            <span class="mono small" class:muted={!ml.fallback} class:warn-text={ml.fallback} title={ml.title}>{ml.text}</span>
          {/if}
        </div>
      {/if}
      <textarea
        class="mono"
        rows="7"
        bind:value={message}
        oninput={() => (edited = true)}
        onkeydown={onkey}
        placeholder="feat(inventory): add cn-035 in GC5A-6Z&#10;&#10;- why, in a few bullets"
        aria-label="Commit message"
      ></textarea>
    </div>
    {#if subject && !conventional}
      <p class="hint warn-text">Use a Conventional Commit subject, for example <code>feat(inventory): …</code>. The CI checks it.</p>
    {/if}
    {#if error}<div class="error-box">{error}</div>{/if}
    <div class="row">
      <button class="btn" onclick={suggest} disabled={suggesting || paths.length === 0}>
        <Icon name="sparkle" size={15} />{suggesting ? 'Asking…' : suggested ? 'Suggest again' : 'Suggest message'}
      </button>
      <button class="btn dark" onclick={commit} disabled={!canCommit}>
        <Icon name="check" size={15} />{committing ? 'Committing…' : `Commit ${paths.length} file${paths.length === 1 ? '' : 's'}`}
      </button>
      <span class="muted small kbd">Ctrl+Enter</span>
    </div>
  </section>

  <div class="callout llm">
    <Icon name="sparkle" size={18} />
    <span>The advisor reads the staged diff and the change intent. It writes nothing: the admin edits and commits.</span>
  </div>

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
        <button class="btn" onclick={checkCommit} disabled={checking}><Icon name="sparkle" size={15} />{checking ? 'Checking…' : 'Check this commit'}</button>
        <button class="btn" onclick={() => actions.runLab(change.id)} disabled={actions.isBusy(change.id)}><Icon name="flask" size={15} />Run lab</button>
        <button class="btn" onclick={() => actions.push(change.id)} disabled={actions.isBusy(change.id)}>
          <Icon name="upload" size={15} />{change.mr ? 'Push' : 'Push, open draft MR'}
        </button>
        <a class="btn" href={href(`/changes/${change.id}`)}>Back to change</a>
      </div>
      {#if check}<AdvisorResult result={check} />{/if}
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
  .sugline {
    display: flex;
    gap: 6px;
    align-items: center;
    padding: 10px 14px 0;
    color: var(--info);
    font-weight: 600;
    font-size: 14px;
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
    border-left: 4px solid var(--info);
    background: var(--panel);
    padding: 12px 16px;
    color: var(--ink);
  }
  .callout.llm :global(svg) {
    color: var(--info);
    flex: none;
  }
  .done > .row:first-child :global(svg) {
    color: var(--ok);
  }
  .accent {
    color: var(--accent);
  }
</style>
