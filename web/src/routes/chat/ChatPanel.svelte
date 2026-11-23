<script lang="ts">
  // Chat in the side panel (same conversation as the chat page).
  import ChatThread from '../../lib/components/chat/ChatThread.svelte';
</script>

<div class="panel-chat">
  <ChatThread />
</div>

<style>
  /* Fill the panel body, so the messages scroll and the composer stays at
     the bottom. */
  .panel-chat {
    position: absolute;
    inset: 10px 12px;
    min-height: 0;
  }
</style>
