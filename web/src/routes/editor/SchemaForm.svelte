<script lang="ts">
  // Form view of a file: generated from the kind's JSON Schema and its x-pk
  // hints. Each edit changes the YAML text (see yamlEdit), which the page
  // saves to the worktree.
  import type { Problem, SecretField } from '../../lib/api/types';
  import { buildForm } from '../../lib/change/formModel';
  import type { JS } from '../../lib/change/schema';
  import { formatPath, type Path } from '../../lib/change/yamlEdit';
  import FormRow from './FormRow.svelte';

  let {
    schema,
    data,
    problems,
    readOnly,
    changeId,
    secrets,
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
    onset: (path: Path, value: unknown) => void;
    onappend: (path: Path, value: unknown) => void;
    ondelete: (path: Path) => void;
  } = $props();

  const sections = $derived(buildForm(schema, data));
  // Problems that no row shows (for example a missing required field).
  const shown = $derived(new Set(sections.flatMap((s) => s.rows.map((r) => formatPath(r.path)))));
  const loose = $derived(problems.filter((p) => !p.field || ![...shown].some((f) => p.field === f || p.field!.startsWith(f + '.') || p.field!.startsWith(f + '['))));
</script>

<div class="form">
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
