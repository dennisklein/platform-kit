<script lang="ts">
  // Chat in the side panel (same conversation as the chat page).
  import ChatThread from '../../lib/components/chat/ChatThread.svelte';
</script>

<div class="panel-chat">
  <ChatThread />
</div>

<style>
  .panel-chat {
    height: 100%;
    min-height: 0;
  }
</style>
