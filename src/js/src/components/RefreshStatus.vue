<!--
Shows whether the current page's data is loading or when it was last
loaded, with a button to reload it. Pages opt in with createPageLoader.
-->
<template>
  <div v-if="pageStatus.refresh" class="refresh-status" aria-live="polite">
    <span class="refresh-label">{{ label }}</span>
    <b-tooltip
      label="Refresh this page's data"
      type="is-light"
      position="is-bottom">
      <button
        class="button is-small is-light"
        aria-label="Refresh this page's data"
        :disabled="pageStatus.loading"
        @click="pageStatus.refresh()">
        <b-icon
          icon="sync-alt"
          size="is-small"
          :custom-class="pageStatus.loading ? 'fa-spin' : ''"></b-icon>
      </button>
    </b-tooltip>
  </div>
</template>

<script>
  import { pageStatus } from '@/utils/pageLoader.js';

  export default {
    data() {
      return { pageStatus, now: Date.now() };
    },
    created() {
      // keeps "updated … ago" current
      this.timer = setInterval(() => (this.now = Date.now()), 15000);
    },
    beforeUnmount() {
      clearInterval(this.timer);
    },
    computed: {
      label() {
        if (this.pageStatus.loading) {
          return this.pageStatus.updatedAt ? 'Refreshing…' : 'Loading…';
        }
        if (this.pageStatus.failed) return 'Refresh failed';
        if (!this.pageStatus.updatedAt) return '';

        const seconds = Math.max(
          0,
          Math.round((this.now - this.pageStatus.updatedAt) / 1000),
        );
        if (seconds < 60) return 'Updated just now';
        const minutes = Math.round(seconds / 60);
        if (minutes < 60) return `Updated ${minutes} min ago`;
        return `Updated ${new Date(this.pageStatus.updatedAt).toLocaleTimeString()}`;
      },
    },
    watch: {
      'pageStatus.updatedAt'() {
        this.now = Date.now();
      },
    },
  };
</script>

<style scoped>
  .refresh-status {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .refresh-label {
    font-size: 0.85rem;
    opacity: 0.8;
    white-space: nowrap;
  }
</style>
