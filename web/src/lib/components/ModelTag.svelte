<script lang="ts">
  // The model behind an AI answer, in one style on every AI surface (chat
  // header and footer, advisor card, commit panel, LLM log): mono, AI teal
  // on soft teal. Short and long forms come from lib/f/llm.ts, for example
  // "built-in", "built-in · from project data", "built-in · secret guard",
  // "from project data" or a model name such as "qwen2.5:7b".
  // fallback: a model is configured but the project data answered; the
  // border turns dashed so that the tag still reads as AI, without alarm.
  let {
    text,
    title = undefined,
    fallback = false,
  }: { text: string; title?: string; fallback?: boolean } = $props();
</script>

{#if text}<span class="mtag" class:fallback {title}>{text}</span>{/if}

<style>
  .mtag {
    display: inline-block;
    max-width: 100%;
    font: 500 11px/1.6 var(--mono);
    letter-spacing: 0;
    text-transform: none;
    color: var(--ai);
    background: var(--ai-soft);
    border: 1px solid var(--ai-line);
    border-radius: 4px;
    padding: 0 6px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    vertical-align: middle;
    flex: none;
  }
  .mtag.fallback {
    border-style: dashed;
  }
</style>
