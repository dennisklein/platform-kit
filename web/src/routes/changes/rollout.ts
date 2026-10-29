// Rollout of a merged change on one cluster, as steps: Flux fetches the
// merge commit, applies the rendered objects, the Kustomizations get
// Ready, the health checks pass (verified).
import type { Cluster, ClusterRollout } from '../../lib/api/types';

export type RolloutStatus = 'done' | 'now' | 'todo' | 'fail';
export interface RolloutStep {
  key: string;
  title: string;
  detail?: string;
  status: RolloutStatus;
}

const short = (sha: string) => sha.slice(0, 7);

/** The steps of one cluster. cl is the cluster status from /clusters (may lag or be missing). */
export function rolloutSteps(r: ClusterRollout, cl: Cluster | undefined, mergeSha: string): RolloutStep[] {
  const has = (sha?: string) => !!sha && !!mergeSha && sha === mergeSha;
  const kusts = cl?.kustomizations ?? [];
  const onMerge = kusts.filter((k) => !!mergeSha && (k.revision ?? '').includes(mergeSha));
  const readyK = r.verified ? kusts.length : onMerge.filter((k) => k.ready).length;
  const fetched = r.deployed || r.verified || has(cl?.pending?.sha) || has(cl?.applied?.sha);
  const failed = !r.verified && has(cl?.applied?.sha) && cl?.healthy === false && cl?.failing === true;
  const objects = cl?.objects;

  const steps: Omit<RolloutStep, 'status'>[] = [
    { key: 'fetch', title: `Flux fetched main@${short(mergeSha || r.sha || '')}`, detail: fetched ? undefined : 'waiting for the next poll' },
    {
      key: 'apply',
      title: r.deployed && objects ? `Applied ${objects} objects` : 'Apply the rendered objects',
      detail: has(cl?.pending?.sha) && !r.deployed ? 'reconciling' : undefined,
    },
    {
      key: 'ready',
      title: kusts.length ? `Kustomizations ${readyK} of ${kusts.length} ready` : 'Kustomizations ready',
      detail: r.deployed && !r.verified ? 'health checks running' : undefined,
    },
    { key: 'verified', title: r.verified ? 'Verified: health checks pass' : 'Verified', detail: undefined },
  ];
  const done = [fetched, r.deployed || r.verified, r.verified, r.verified];
  const current = done.findIndex((d) => !d);
  return steps.map((s, i) => {
    let status: RolloutStatus = done[i] ? 'done' : i === current ? 'now' : 'todo';
    if (failed && i === 2) {
      status = 'fail';
      return { ...s, status, detail: cl?.message ?? 'health checks failed' };
    }
    if (failed && i > 2) status = 'todo';
    return { ...s, status };
  });
}
