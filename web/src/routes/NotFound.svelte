<script lang="ts">
  import { router, href, type Params } from '../lib/router.svelte';

  let { params, query }: { params: Params; query: URLSearchParams } = $props();
</script>

<section class="section nf">
  <h1>Page not found</h1>
  <p class="muted">No page for <span class="mono">{router.path}</span>.</p>
  <p><a href={href('/changes')}>Go to changes</a></p>
</section>

<style>
  .nf {
    margin: 16px;
    display: grid;
    gap: 8px;
  }
  p {
    margin: 0;
  }
</style>
