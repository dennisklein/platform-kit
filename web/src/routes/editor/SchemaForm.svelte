<script lang="ts">
  // Form view of a file: generated from the kind's JSON Schema and its x-pk
  // hints. Each edit changes the YAML text (see yamlEdit), which the page
  // saves to the worktree.
  import type { Problem, SecretField } from '../../lib/api/types';
  import { buildForm } from '../../lib/change/formModel';
  import type { JS } from '../../lib/change/schema';
  import { formatPath, type Path } from '../../lib/change/yamlEdit';
  import { tick } from 'svelte';
  import FormRow from './FormRow.svelte';

  let {
    schema,
    data,
    problems,
    readOnly,
    changeId,
    secrets,
    focus = null,
    onset,
    onappend,
    ondelete,
  }: {
    schema: JS;
    data: unknown;
    problems: Problem[];
    readOnly: boolean;
    changeId?: string;
    secrets: SecretField[];
    focus?: { field: string; seq: number } | null;
    onset: (path: Path, value: unknown) => void;
    onappend: (path: Path, value: unknown) => void;
    ondelete: (path: Path) => void;
  } = $props();

  // Scroll to a field (a deep link to a line), highlight it and put the
  // cursor in its first input. The closest row wins: spec.ports[2].peer.port
  // lands on the peer picker of port p2.
  let formEl = $state<HTMLDivElement | undefined>(undefined);
  $effect(() => {
    const f = focus;
    const root = formEl;
    if (!f || !root) return;
    void tick().then(() => {
      let best: HTMLElement | null = null;
      for (const el of root.querySelectorAll<HTMLElement>('[data-field]')) {
        const d = el.dataset.field ?? '';
        const hit = f.field === d || f.field.startsWith(d + '.') || f.field.startsWith(d + '[');
        if (hit && (!best || d.length > (best.dataset.field ?? '').length)) best = el;
      }
      if (!best) return;
      best.scrollIntoView({ block: 'center', behavior: 'smooth' });
      best.classList.remove('flashrow');
      void best.offsetWidth; // restart the animation
      best.classList.add('flashrow');
      const el = best;
      setTimeout(() => el.classList.remove('flashrow'), 2400);
      el.querySelector<HTMLElement>('input:not([readonly]), select:not([disabled])')?.focus({ preventScroll: true });
    });
  });

  const sections = $derived(buildForm(schema, data));
  // Problems that no row shows (for example a missing required field).
  const shown = $derived(new Set(sections.flatMap((s) => s.rows.map((r) => formatPath(r.path)))));
  const loose = $derived(problems.filter((p) => !p.field || ![...shown].some((f) => p.field === f || p.field!.startsWith(f + '.') || p.field!.startsWith(f + '['))));
</script>

<div class="form" bind:this={formEl}>
  {#each sections as s (s.title)}
    <section>
      <h3 class="eyebrow">{s.title}</h3>
      {#each s.rows as row (formatPath(row.path))}
        <FormRow {row} root={schema} {data} {problems} {readOnly} {changeId} {secrets} {onset} {onappend} {ondelete} />
      {/each}
    </section>
  {:else}
    <p class="muted">The schema has no fields for the form. Use YAML.</p>
  {/each}
  {#if loose.length}
    <section>
      <h3 class="eyebrow">Other problems</h3>
      {#each loose as p, i (i)}
        <div class="loose {p.severity}">{p.line ? `line ${p.line}: ` : ''}{p.message}</div>
      {/each}
    </section>
  {/if}
</div>

<style>
  /* FormRow stacks label and input when the form is narrow. */
  .form {
    container-type: inline-size;
    padding: 14px 16px 40px;
    display: grid;
    gap: 18px;
    max-width: 900px;
  }
  section {
    display: grid;
  }
  h3 {
    border-bottom: 1px solid var(--line);
    padding-bottom: 8px;
    margin-bottom: 6px;
  }
  /* A row reached by a deep link: a short highlight. */
  .form :global(.flashrow) {
    animation: flashrow 2.2s ease-out;
    border-radius: 6px;
  }
  @keyframes flashrow {
    0%,
    35% {
      background: color-mix(in srgb, var(--accent-fill) 26%, transparent);
      box-shadow: 0 0 0 4px color-mix(in srgb, var(--accent-fill) 26%, transparent);
    }
    100% {
      background: transparent;
      box-shadow: 0 0 0 4px transparent;
    }
  }
  .loose {
    font-size: 13px;
    padding: 3px 0;
  }
  .loose.error {
    color: var(--bad);
  }
  .loose.warning {
    color: var(--warn);
  }
</style>
