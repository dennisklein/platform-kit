<script lang="ts">
  // History with one merge selected (/history/<sha>), filters kept in the query.
  import type { Params } from '../../lib/router.svelte';
  import HistoryBrowser from '../../lib/components/history/HistoryBrowser.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();
</script>

<HistoryBrowser {query} sha={params.sha} />
