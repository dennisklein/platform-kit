// The generated catalogs (metal/hack/docgen writes them to src/data/generated) and the
// anchor rules of the catalog pages. A deep link uses these ids:
//   [Available](ref:phases#host-available)
//   [Verified](ref:conditions#verified), [SerialMismatch](ref:conditions#reason-serialmismatch)
//   [](ref:labels-annotations#metal.pk.io/hold)
import phasesJson from '../../data/generated/phases.json';
import conditionsJson from '../../data/generated/conditions.json';
import keysJson from '../../data/generated/keys.json';
import { branch, repoUrl } from '../../../scripts/site.mjs';

export interface PhaseSet {
  type: string;
  group: string;
  source: string;
  description: string;
  kinds: string[];
  usedBy: string[];
  values: { name: string; value: string; description: string }[];
}
export interface Condition {
  name: string;
  value: string;
  group: string;
  source: string;
  description: string;
}
export interface Reason extends Condition {
  on?: { condition: string; status: string }[];
  context?: string;
}
export interface Key {
  key: string;
  kind: 'annotation' | 'label' | 'finalizer';
  exported: boolean;
  description: string;
  constants: { name: string; source: string }[];
}

export const phaseSets = phasesJson.phases as PhaseSet[];
export const conditions = conditionsJson.conditions as Condition[];
export const reasons = conditionsJson.reasons as Reason[];
export const operationReasons = conditionsJson.operationReasons as Condition[];
export const keys = keysJson.keys as Key[];

/** The phase set of a kind: `phaseSet('Host')`. */
export function phaseSet(kind: string): PhaseSet {
  const set = phaseSets.find((p) => p.kinds.includes(kind));
  if (!set) throw new Error(`catalog: no phase set for kind "${kind}" in phases.json`);
  return set;
}

/** `host-available`: the kind and the phase, in lower case. */
export const phaseId = (kind: string, value: string) => `${kind.toLowerCase()}-${value.toLowerCase()}`;

/**
 * The id of a name that two API groups can have. The name in lower case is the id.
 * When metal.pk.io and os.pk.io both have the name, the os.pk.io one gets `os-`.
 */
function ider<T extends { value: string; group: string }>(all: T[], prefix: string) {
  const groups = new Map<string, Set<string>>();
  for (const x of all) groups.set(x.value, (groups.get(x.value) ?? new Set()).add(x.group));
  return (x: { value: string; group: string }) => {
    const both = (groups.get(x.value)?.size ?? 0) > 1;
    return `${prefix}${both && x.group !== 'metal.pk.io' ? 'os-' : ''}${x.value.toLowerCase()}`;
  };
}
export const conditionId = ider(conditions, '');
export const reasonId = ider(reasons, 'reason-');
export const operationReasonId = (r: { value: string }) => `opreason-${r.value.toLowerCase()}`;
/** The id of a key is the key: `metal.pk.io/hold`. */
export const keyId = (k: { key: string }) => k.key;

/** The condition that a reason names, in the same group if that group has it. */
export function conditionOf(name: string, group: string): Condition | undefined {
  return conditions.find((c) => c.value === name && c.group === group) ?? conditions.find((c) => c.value === name);
}

/** Go comments start in lower case or with the name of the constant. Make a sentence. */
export function sentence(text: string, name?: string, noun?: string): string {
  let t = (text ?? '').trim();
  if (!t) return '';
  if (name) {
    const lower = name[0].toLowerCase() + name.slice(1);
    for (const n of [name, lower]) {
      if (t.startsWith(`${n} is `)) t = t.slice(n.length + 4);
      else if (t.startsWith(`${n} `) && noun) t = `${noun} ${t.slice(n.length + 1)}`;
    }
  }
  return t[0].toUpperCase() + t.slice(1);
}

/** The GitHub address of a source file in the repository. */
export const sourceUrl = (path: string) => `${repoUrl}/blob/${branch}/${path}`;
