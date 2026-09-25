<template>
  <div>
    <template v-if="pid != 0">
      <Terminal :wsPath="terminalPath" :resizePath="resizePath" />
    </template>
    <template v-else>
      <section class="hero is-light is-bold is-large">
        <div class="hero-body">
          <div class="container" style="text-align: center">
            <h1 class="title">{{ message }}</h1>
          </div>
        </div>
      </section>
    </template>
  </div>
</template>

<script>
  import Terminal from '@/components/MiniTerminal.vue';
  import axiosInstance from '@/utils/axios.js';

  export default {
    components: {
      Terminal,
    },

    data() {
      return {
        pid: 0,
        message: 'Starting console…',
      };
    },

    computed: {
      terminalPath() {
        return this.$router.resolve({
          name: 'console-ws',
          params: { pid: this.pid },
        }).href;
      },

      resizePath() {
        // relative to the axios instance's api/v1 base; a router-resolved
        // (base-prefixed) path would get the baseURL prepended again
        return `console/${this.pid}/size`;
      },
    },
    mounted() {
      axiosInstance
        .post('console')
        .then((resp) => {
          this.pid = resp.data.pid;
        })
        .catch((err) => {
          switch (err.response?.status) {
            // the server was started without --minimega-console
            case 405:
              this.message = 'Console access is not configured.';
              break;
            case 403:
              this.message = 'You do not have access to the console.';
              break;
            default:
              this.message = 'Could not start the console.';
              console.warn('failed to start the console', err);
          }
        });
    },
  };
</script>
