<script lang="ts">
  // Chat page: the conversation in full width, with what the chat can do.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { session } from '../../lib/stores/me.svelte';
  import ChatThread from '../../lib/components/chat/ChatThread.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const llm = $derived(session.status?.llm);
</script>

<div class="chatpage">
  <section class="conv">
    <header>
      <Glyph name="chat" size={18} />
      <h1>Chat</h1>
      <span class="spacer"></span>
      <span class="as">as {session.me?.id ?? '…'}</span>
    </header>
    <div class="threadwrap"><ChatThread wide /></div>
  </section>
  <aside class="about">
    <section class="card">
      <h3>Model</h3>
      <p class="mono">{llm ? (llm.mock ? 'built-in analysis (no model endpoint)' : llm.model) : '…'}</p>
      {#if llm?.baseUrl}<p class="muted small mono">{llm.baseUrl}</p>{/if}
      {#if llm?.embedModel}<p class="muted small">Embeddings: <span class="mono">{llm.embedModel}</span></p>{/if}
    </section>
    <section class="card">
      <h3>What it can do</h3>
      <ul>
        <li>Read the state repo, the KB, merge trailers, MR discussions and lab reports.</li>
        <li>Answer with sources you can open.</li>
        <li>Suggest edits as a diff in your change. You accept or reject them.</li>
      </ul>
      <h3 class="no">What it never does</h3>
      <ul>
        <li>Commit, run labs or mark ready on its own.</li>
        <li>See secrets: SOPS values and secret fields are removed before each call.</li>
        <li>Approve or merge.</li>
      </ul>
    </section>
    <section class="card">
      <h3>Every call is logged</h3>
      <p class="small">Prompts (redacted), answers, sources and your ratings go to the <a href={href('/llm-log')}>LLM log</a>.</p>
    </section>
  </aside>
</div>

<style>
  .chatpage {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 320px;
    height: 100%;
    min-height: 0;
  }
  .conv {
    display: grid;
    grid-template-rows: auto minmax(0, 1fr);
    padding: 14px 24px 12px;
    min-height: 0;
    gap: 10px;
  }
  header {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  header :global(svg) {
    color: var(--accent);
  }
  .as {
    font-weight: 600;
  }
  .threadwrap {
    min-height: 0;
    max-width: 920px;
    width: 100%;
    margin: 0 auto;
  }
  .about {
    border-left: 1px solid var(--line);
    padding: 14px;
    display: grid;
    gap: 12px;
    align-content: start;
    overflow-y: auto;
  }
  .card {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    padding: 10px 14px;
  }
  .card h3 {
    margin-bottom: 6px;
  }
  .card h3.no {
    margin-top: 10px;
  }
  ul {
    margin: 0;
    padding-left: 18px;
    font-size: 13px;
    display: grid;
    gap: 3px;
  }
  p {
    margin: 0;
  }
  @media (max-width: 1000px) {
    .chatpage {
      grid-template-columns: 1fr;
    }
    .about {
      display: none;
    }
  }
</style>
