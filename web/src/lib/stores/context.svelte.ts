// The change the user works on, for pages outside a change (assets, KB,
// chat): the last change route visited, else my newest local draft.
import { router } from '../router.svelte';
import { changes } from './changes.svelte';
import { session } from './me.svelte';

const KEY = 'pk.workChange';
const DONE = ['merged', 'deployed', 'verified', 'closed'];

function load(): string | null {
  try {
    return sessionStorage.getItem(KEY);
  } catch {
    return null;
  }
}

class WorkContext {
  #last = $state<string | null>(load());

  /** Remember change ids from the route (runs in an effect below). */
  track() {
    const id = router.params.id;
    if (id && /^c-[a-z0-9]{6}$/.test(id) && id !== this.#last) {
      this.#last = id;
      try {
        sessionStorage.setItem(KEY, id);
      } catch {
        // private mode
      }
    }
  }

  /** The last visited change, while it is still open. */
  get visited(): string | undefined {
    const id = this.#last;
    if (!id) return undefined;
    const c = changes.byId(id);
    return c && !DONE.includes(c.state) ? id : undefined;
  }

  /** The open change id to use as context, if any. */
  get changeId(): string | undefined {
    const open = changes.list.filter((c) => !DONE.includes(c.state));
    if (this.#last && open.some((c) => c.id === this.#last)) return this.#last;
    const me = session.me?.id;
    const mine = open
      .filter((c) => c.local && (c.author === me || c.group === 'mine'))
      .sort((a, b) => (b.updatedAt ?? '').localeCompare(a.updatedAt ?? ''));
    return mine[0]?.id;
  }

  /**
   * The ref a page shows: ?ref= if set, main when the user picked main
   * (?pinref), else the change the user came from (a change page visited in
   * this tab). An old draft never takes over the asset pages on its own.
   */
  refFor(query: URLSearchParams): string {
    return query.get('ref') ?? (query.has('pinref') ? 'main' : (this.visited ?? 'main'));
  }
}

export const work = new WorkContext();

// Follow the route for the life of the app.
$effect.root(() => {
  $effect(() => work.track());
});
