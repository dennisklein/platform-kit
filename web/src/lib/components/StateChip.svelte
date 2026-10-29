<script lang="ts" module>
  import type { ChangeState, LabState } from '../api/types';

  export type ChipKind = ChangeState | 'labfail' | 'labrun';

  /** Chip kind of a change: a failed or running lab run shows on draft and lab. */
  export function chipKind(state: ChangeState, labState?: LabState): ChipKind {
    if (state === 'draft' || state === 'lab') {
      if (labState === 'fail') return 'labfail';
      if (labState === 'running') return 'labrun';
    }
    return state;
  }

  const LABELS: Record<ChipKind, string> = {
    draft: 'draft',
    lab: 'lab',
    labfail: 'lab failed',
    labrun: 'lab running',
    review: 'review',
    approved: 'approved',
    merged: 'merged',
    deployed: 'deployed',
    verified: 'verified',
    closed: 'closed',
  };

  const TONE: Record<ChipKind, string> = {
    draft: 'idle',
    closed: 'idle',
    lab: 'warn',
    labrun: 'warn',
    labfail: 'bad',
    review: 'info',
    approved: 'info',
    merged: 'ok',
    deployed: 'ok',
    verified: 'ok',
  };
</script>

<script lang="ts">
  let {
    state,
    labState,
    label,
    short = false,
  }: { state: ChangeState; labState?: LabState; label?: string; short?: boolean } = $props();

  const kind = $derived(chipKind(state, labState));
  const text = $derived(label ?? (short && kind === 'labrun' ? 'lab' : LABELS[kind]));
</script>

<!-- The box and the colours are the shared .schip in app.css (ResultChip
     uses the same), so a state looks the same on every page. -->
<span class="schip tone-{TONE[kind]} s-{kind}" data-state={kind}
  >{#if kind === 'labrun'}<span class="sdot live" aria-hidden="true"></span>{/if}{text}</span
>
