<script lang="ts">
  // The chat conversation: questions as dennis, answers with sources,
  // suggestion cards (diff, Accept/Reject), refusals for secrets, thumbs.
  import { tick } from 'svelte';
  import { chat, type ChatEntry } from '../../stores/chat.svelte';
  import { session } from '../../stores/me.svelte';
  import { changes } from '../../stores/changes.svelte';
  import { href } from '../../router.svelte';
  import type { Suggestion } from '../../api/ftypes';
  import SourceChip from '../advisor/SourceChip.svelte';
  import SuggestionCard from '../advisor/SuggestionCard.svelte';
  import Thumbs from '../advisor/Thumbs.svelte';
  import Markdown from '../Markdown.svelte';
  import Glyph from '../Glyph.svelte';
  import { modelLabel, type AnswerLike } from '../../f/llm';

  // "LLM offline · answer from data" when the bench data answered instead of the model.
  function answerModel(a: AnswerLike) {
    const llm = session.status?.llm;
    return modelLabel(a, !!llm && !llm.mock && !llm.reachable);
  }

  let { wide = false }: { wide?: boolean } = $props();

  let text = $state('');
  let list: HTMLDivElement | undefined = $state();
  let input: HTMLTextAreaElement | undefined = $state();

  const EXAMPLES = [
    'Why does cn-003 use port 37 on data-a?',
    'What is the hubble UI token?',
    'Add cn-037 to the rack-6z BGP policy',
  ];

  async function send(t = text) {
    if (!t.trim()) return;
    text = '';
    const p = chat.send(t);
    await tick();
    scroll();
    await p;
    await tick();
    scroll();
  }

  function scroll() {
    list?.scrollTo({ top: list.scrollHeight, behavior: 'smooth' });
  }

  $effect(() => {
    void chat.entries.length;
    void tick().then(() => list?.scrollTo({ top: list.scrollHeight }));
  });

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      void send();
    }
  }

  function decided(e: ChatEntry, i: number, s: Suggestion) {
    if (!e.answer) return;
    const suggestions = e.answer.suggestions.map((x, j) => (j === i ? s : x));
    chat.update(e.id, { ...e.answer, suggestions });
  }

  const open = $derived(changes.list.filter((c) => !['merged', 'deployed', 'verified', 'closed'].includes(c.state)));
  const ctx = $derived(chat.changeId);
  const ctxTitle = $derived(changes.byId(ctx)?.title);

  function pick(e: Event) {
    const v = (e.target as HTMLSelectElement).value;
    chat.picked = v === '__none' ? null : v === '__auto' ? undefined : v;
  }
</script>

<div class="thread" class:wide>
  <div class="ctx">
    <Glyph name="branch" size={13} />
    <select class="ctxsel" onchange={pick} aria-label="Change for suggestions" title={ctxTitle ?? 'No change: answers only, no suggested edits'}>
      <option value="__auto" selected={chat.picked === undefined}>{ctx && chat.picked === undefined ? `${ctx} (current)` : 'current change'}</option>
      {#each open as c (c.id)}
        <option value={c.id} selected={chat.picked === c.id}>{c.id} · {c.title}</option>
      {/each}
      <option value="__none" selected={chat.picked === null}>no change</option>
    </select>
    <span class="spacer"></span>
    {#if chat.entries.length}
      <button class="btn ghost small" onclick={() => chat.clear()} title="Clear the conversation">Clear</button>
    {/if}
  </div>

  <div class="msgs" bind:this={list} aria-live="polite">
    {#each chat.entries as e (e.id)}
      {#if e.role === 'user'}
        <div class="q">{e.content}</div>
      {:else if e.error}
        <div class="a err"><Glyph name="alert" size={14} /> {e.error}</div>
      {:else if e.answer}
        {@const a = e.answer}
        <div class="a" class:refused={a.refused}>
          {#if a.refused}<span class="lock"><Glyph name="lock" size={14} /></span>{/if}
          <div class="txt"><Markdown source={a.text} /></div>
          {#if a.sources.length}
            <div class="srcs">
              {#each a.sources as s, i (i)}<SourceChip source={s} changeId={e.changeId} />{/each}
            </div>
          {/if}
          {#each a.suggestions as s, i (s.id)}
            <div class="sg">
              <SuggestionCard suggestion={s} onDecided={(x) => decided(e, i, x)} />
            </div>
          {/each}
          <div class="foot">
            <Thumbs logId={a.logId} compact note={false} />
            <span class="model mono" class:warn-text={answerModel(a).fallback} title={answerModel(a).title}>{answerModel(a).text}</span>
            {#if a.logId}<a class="log" href={href('/llm-log', { q: a.logId })} title="Open in the LLM log">log</a>{/if}
          </div>
        </div>
      {/if}
    {:else}
      <div class="intro">
        <p>
          Ask about the project. The chat reads the state repo, the KB, merge trailers, MR discussions and lab reports.
          Edits come back as suggestions in your change; you accept them.
        </p>
        <div class="ex">
          {#each EXAMPLES as x (x)}
            <button class="exq" onclick={() => send(x)}>{x}</button>
          {/each}
        </div>
      </div>
    {/each}
    {#if chat.busy}
      <div class="a typing" aria-label="Thinking"><span></span><span></span><span></span></div>
    {/if}
  </div>

  <form
    class="ask"
    onsubmit={(e) => {
      e.preventDefault();
      void send();
    }}
  >
    <textarea
      bind:this={input}
      bind:value={text}
      {onkeydown}
      rows={wide ? 2 : 1}
      placeholder="Ask about the project…"
      aria-label="Message"
    ></textarea>
    <button class="btn small primary" disabled={!text.trim() || chat.busy} title="Send (Enter)">
      <Glyph name="send" size={13} /><span class="sr-only">Send</span>
    </button>
  </form>
  <p class="who small muted">as {session.me?.id ?? '…'} · secret fields are removed before each call</p>
</div>

<style>
  .thread {
    display: grid;
    grid-template-rows: auto minmax(0, 1fr) auto auto;
    height: 100%;
    min-height: 0;
    gap: 8px;
  }
  .ctx {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--accent);
    min-width: 0;
  }
  .ctxsel {
    border: 0;
    background: transparent;
    font: 500 12px var(--mono);
    color: var(--ink);
    max-width: 240px;
    padding: 2px 0;
  }
  .msgs {
    position: relative;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 2px 2px 8px;
    min-height: 0;
  }
  .q {
    align-self: flex-end;
    max-width: 88%;
    background: var(--gsi-navy);
    color: #fff;
    padding: 8px 12px;
    border-radius: 10px 10px 2px 10px;
    font-size: 14px;
    white-space: pre-wrap;
  }
  .a {
    align-self: flex-start;
    max-width: 96%;
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 10px 10px 10px 2px;
    padding: 9px 12px;
    display: grid;
    gap: 6px;
    font-size: 14px;
    min-width: 0;
  }
  .wide .a {
    max-width: 80%;
  }
  .a.refused {
    border-color: color-mix(in srgb, var(--warn) 45%, transparent);
    background: var(--warn-soft);
    grid-template-columns: auto 1fr;
  }
  .a.refused > :not(.lock, .txt) {
    grid-column: 1 / -1;
  }
  .lock {
    color: var(--warn);
    padding-top: 3px;
  }
  .txt :global(.md) {
    font-size: 14px;
    line-height: 1.5;
  }
  .txt :global(.md p:last-child),
  .txt :global(.md ul:last-child) {
    margin-bottom: 0;
  }
  .a.err {
    background: var(--bad-soft);
    color: var(--bad);
    border-color: transparent;
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .srcs {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .sg {
    border-top: 1px dashed var(--line);
    padding-top: 8px;
  }
  .foot {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: -2px;
  }
  .model {
    font-size: 11px;
    color: var(--muted);
  }
  .log {
    font-size: 11px;
    color: var(--muted);
    margin-left: auto;
  }
  .intro {
    color: var(--muted);
    font-size: 13px;
    display: grid;
    gap: 10px;
  }
  .intro p {
    margin: 0;
  }
  .ex {
    display: grid;
    gap: 6px;
  }
  .exq {
    text-align: left;
    border: 1px solid var(--line);
    background: var(--panel);
    border-radius: 8px;
    padding: 7px 10px;
    font-size: 13px;
    color: var(--ink);
  }
  .exq:hover {
    border-color: var(--accent-fill);
  }
  .typing {
    flex-direction: row;
    display: flex;
    gap: 4px;
    padding: 12px 14px;
  }
  .typing span {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--muted);
    animation: blink 1.2s infinite;
  }
  .typing span:nth-child(2) {
    animation-delay: 0.2s;
  }
  .typing span:nth-child(3) {
    animation-delay: 0.4s;
  }
  @keyframes blink {
    0%,
    80%,
    100% {
      opacity: 0.25;
    }
    40% {
      opacity: 1;
    }
  }
  .ask {
    display: flex;
    gap: 6px;
    align-items: flex-end;
    border: 1px solid var(--line-strong);
    border-radius: 18px;
    padding: 4px 4px 4px 14px;
    background: var(--panel);
  }
  .ask:focus-within {
    border-color: var(--accent-fill);
  }
  textarea {
    flex: 1;
    border: 0;
    outline: none;
    resize: none;
    background: transparent;
    font-size: 14px;
    padding: 5px 0;
    max-height: 120px;
  }
  .ask .btn {
    border-radius: 14px;
    padding: 5px 9px;
  }
  .who {
    margin: -4px 0 0;
    text-align: center;
  }
</style>
