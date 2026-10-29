<script lang="ts">
  // Chat page: the conversation in full width, with what the chat can do.
  import type { Params } from '../../lib/router.svelte';
  import { href } from '../../lib/router.svelte';
  import { session } from '../../lib/stores/me.svelte';
  import ChatThread from '../../lib/components/chat/ChatThread.svelte';
  import Glyph from '../../lib/components/Glyph.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();

  const llm = $derived(session.status?.llm);
  const model = $derived(llm ? (llm.mock ? 'built-in' : llm.model) : '');
</script>

<div class="chatpage">
  <section class="conv">
    <div class="col">
      <header>
        <span class="mark" aria-hidden="true"><Glyph name="sparkle" size={16} /></span>
        <h1>Chat</h1>
        {#if model}<span class="model mono" title={llm?.mock ? 'No model endpoint: answers come from the project data' : llm?.baseUrl}>{model}</span>{/if}
      </header>
      <div class="threadwrap"><ChatThread wide /></div>
    </div>
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
    padding: 14px 24px 12px;
    min-height: 0;
    display: flex;
  }
  /* Title, thread and composer share one column. */
  .col {
    display: grid;
    grid-template-rows: auto minmax(0, 1fr);
    gap: 10px;
    max-width: 920px;
    width: 100%;
    margin: 0 auto;
    min-height: 0;
  }
  header {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .mark {
    width: 28px;
    height: 28px;
    border-radius: 50%;
    display: grid;
    place-items: center;
    background: var(--ai-fill);
    color: var(--ai-on);
  }
  h1 {
    margin: 0;
  }
  .model {
    font-size: 12px;
    padding: 1px 8px;
    border-radius: 999px;
    border: 1px solid var(--ai-line);
    background: var(--ai-soft);
    color: var(--ai);
  }
  .threadwrap {
    min-height: 0;
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
