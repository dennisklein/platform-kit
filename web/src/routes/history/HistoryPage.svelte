<script lang="ts">
  // History of main (?entity=cn-003&author=&path=&kind=&q=).
  import type { Params } from '../../lib/router.svelte';
  import HistoryBrowser from '../../lib/components/history/HistoryBrowser.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();
</script>

<HistoryBrowser {query} />
