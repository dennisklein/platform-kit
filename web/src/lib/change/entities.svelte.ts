// Entity names per kind (select options for x-pk-ref fields) and rack
// occupancy for the rack picker. Cached per ref; refresh() after saves.
import { api, must } from '../api/client';
import type { components } from '../api/schema';
import type { EntitySummary, Rack } from '../api/types';

type Entity = components['schemas']['Entity'];

/** Value at a dotted path ("spec.model") in plain data. */
function valueAt(data: unknown, dotted: string): unknown {
  let cur: unknown = data;
  for (const k of dotted.split('.')) {
    if (cur === null || typeof cur !== 'object') return undefined;
    cur = (cur as Record<string, unknown>)[k];
  }
  return cur;
}

class EntityCache {
  #lists = $state<Record<string, EntitySummary[]>>({});
  #racks = $state<Record<string, Rack | null>>({});
  #details = $state<Record<string, Entity | null>>({});
  #values = $state<Record<string, string[]>>({});
  #pending = new Set<string>();

  /** One entity at ref, with its fields (derived ones computed by the bench). */
  detail(kind: string, name: string, ref?: string): Entity | null {
    if (!kind || !name) return null;
    const key = `${kind}/${name}@${ref ?? 'main'}`;
    if (!(key in this.#details) && !this.#pending.has(key)) {
      this.#pending.add(key);
      void must(api.GET('/entities/{kind}/{name}', { params: { path: { kind, name }, query: { ref } } }))
        .then((e) => (this.#details = { ...this.#details, [key]: e }))
        .catch(() => (this.#details = { ...this.#details, [key]: null }))
        .finally(() => this.#pending.delete(key));
    }
    return this.#details[key] ?? null;
  }

  /**
   * Values that other entities of a kind share for a field (for example
   * the hardware model), most common first. Values seen once (serials)
   * are left out. Reads a sample of the entities at main.
   */
  values(kind: string, field: string): string[] {
    const key = `${kind}:${field}`;
    if (!(key in this.#values) && !this.#pending.has(key)) {
      this.#pending.add(key);
      void (async () => {
        const list = await must(api.GET('/entities', { params: { query: { kind } } }));
        const sample = list.slice(-8);
        const docs = await Promise.all(
          sample.map((e) =>
            must(api.GET('/entities/{kind}/{name}', { params: { path: { kind, name: e.name } } })).catch(() => null),
          ),
        );
        const n = new Map<string, number>();
        for (const d of docs) {
          const v = valueAt(d?.data, field);
          if (typeof v === 'string' && v) n.set(v, (n.get(v) ?? 0) + 1);
        }
        return [...n.entries()].filter(([, k]) => k > 1).sort((a, b) => b[1] - a[1]).map(([v]) => v);
      })()
        .then((v) => (this.#values = { ...this.#values, [key]: v }))
        .catch(() => (this.#values = { ...this.#values, [key]: [] }))
        .finally(() => this.#pending.delete(key));
    }
    return this.#values[key] ?? [];
  }

  /** Entities of a kind at ref (main or a change id). Loads on first use. */
  list(kind: string, ref?: string): EntitySummary[] {
    const key = `${kind}@${ref ?? 'main'}`;
    if (!(key in this.#lists) && !this.#pending.has(key)) {
      this.#pending.add(key);
      void must(api.GET('/entities', { params: { query: { kind, ref } } }))
        .then((l) => (this.#lists = { ...this.#lists, [key]: l }))
        .catch(() => (this.#lists = { ...this.#lists, [key]: [] }))
        .finally(() => this.#pending.delete(key));
    }
    return this.#lists[key] ?? [];
  }

  rack(name: string, ref?: string): Rack | null {
    if (!name) return null;
    const key = `${name}@${ref ?? 'main'}`;
    if (!(key in this.#racks) && !this.#pending.has(key)) {
      this.#pending.add(key);
      void must(api.GET('/racks/{name}', { params: { path: { name }, query: { ref } } }))
        .then((r) => (this.#racks = { ...this.#racks, [key]: r }))
        .catch(() => (this.#racks = { ...this.#racks, [key]: null }))
        .finally(() => this.#pending.delete(key));
    }
    return this.#racks[key] ?? null;
  }

  /** Drop cached racks (and lists of a ref) after a save. */
  refresh(ref?: string) {
    const suffix = `@${ref ?? 'main'}`;
    this.#racks = Object.fromEntries(Object.entries(this.#racks).filter(([k]) => !k.endsWith(suffix)));
    this.#details = Object.fromEntries(Object.entries(this.#details).filter(([k]) => !k.endsWith(suffix)));
    this.#lists = Object.fromEntries(Object.entries(this.#lists).filter(([k]) => !k.endsWith(suffix) || k.startsWith('Rack@') || k.startsWith('Switch@') || k.startsWith('Cluster@')));
  }
}

export const entities = new EntityCache();

/** Free units check for a node at rack/unit/size, ignoring the node itself. */
export function unitInfo(rack: Rack | null, unit: number, size: number, self: string): { ok: boolean; text: string } {
  if (!rack) return { ok: true, text: 'rack picker' };
  if (unit < 1 || unit + size - 1 > rack.units) return { ok: false, text: `rack has U1–U${rack.units}` };
  for (const o of rack.occupants) {
    if (o.name === self) continue;
    const a = o.unit;
    const b = o.unit + o.size - 1;
    if (unit <= b && unit + size - 1 >= a) return { ok: false, text: `U${a}${o.size > 1 ? `–${b}` : ''} in use by ${o.name}` };
  }
  return { ok: true, text: size > 1 ? `U${unit}–${unit + size - 1} free` : `U${unit} free` };
}
