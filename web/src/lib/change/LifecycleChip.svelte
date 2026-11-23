<script lang="ts">
  // The state chip of a change from lifecycleState: the change page, the top
  // bar and the rail show the same word ("lab passed" after a passing lab on
  // the head, before the MR). Same box as StateChip (.schip in app.css).
  import { lifecycleLabel, lifecycleState, lifecycleTone, type LifecycleInput } from './lifecycle';

  let { change, short = false }: { change: LifecycleInput; short?: boolean } = $props();

  const kind = $derived(lifecycleState(change));
  const text = $derived(short && kind === 'labrun' ? 'lab' : lifecycleLabel(kind));
  const title = $derived(kind === 'labpass' ? 'The lab passed on the head. Review is next.' : undefined);
</script>

<span class="schip tone-{lifecycleTone(kind)} s-{kind}" data-state={kind} {title}
  >{#if kind === 'labrun'}<span class="sdot live" aria-hidden="true"></span>{/if}{text}</span
>
