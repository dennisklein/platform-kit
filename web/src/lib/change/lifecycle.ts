// The lifecycle state of a change in one place, so the change page, the top
// bar, the Changes rail and the review page say the same thing. Pure, tested
// with node --test.
import type { ChangeState, LabState } from '../api/types';

/**
 * A change state as people read it: a failed or running lab shows on draft
 * and lab, and a draft whose lab passed on the head is "lab passed" (the lab
 * gate is met, push and mark ready are next), not "draft".
 */
export type LifecycleState = ChangeState | 'labfail' | 'labrun' | 'labpass';

export type LifecycleTone = 'idle' | 'warn' | 'bad' | 'info' | 'ok';

/** The fields lifecycleState reads (ChangeSummary and Change both have them). */
export interface LifecycleInput {
  state: ChangeState;
  labState?: LabState;
}

/** The lifecycle state of a change (see LifecycleState). */
export function lifecycleState(c: LifecycleInput): LifecycleState {
  if (c.state === 'draft' || c.state === 'lab') {
    if (c.labState === 'fail') return 'labfail';
    if (c.labState === 'running') return 'labrun';
    if (c.labState === 'pass') return 'labpass';
  }
  return c.state;
}

const LABELS: Record<LifecycleState, string> = {
  draft: 'draft',
  lab: 'lab',
  labfail: 'lab failed',
  labrun: 'lab running',
  labpass: 'lab passed',
  review: 'review',
  approved: 'approved',
  merged: 'merged',
  deployed: 'deployed',
  verified: 'verified',
  closed: 'closed',
};

const TONES: Record<LifecycleState, LifecycleTone> = {
  draft: 'idle',
  closed: 'idle',
  lab: 'warn',
  labrun: 'warn',
  labfail: 'bad',
  labpass: 'ok',
  review: 'info',
  approved: 'info',
  merged: 'ok',
  deployed: 'ok',
  verified: 'ok',
};

/** The words for a lifecycle state ("lab passed"). */
export function lifecycleLabel(s: LifecycleState): string {
  return LABELS[s];
}

/** The chip tone of a lifecycle state. */
export function lifecycleTone(s: LifecycleState): LifecycleTone {
  return TONES[s];
}
