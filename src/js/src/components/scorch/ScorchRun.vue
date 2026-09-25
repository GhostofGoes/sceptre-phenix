<template>
  <div class="content">
    <div class="columns is-vcentered">
      <div class="column is-2">
        <b-tooltip
          v-if="loop > 0"
          label="return to previous loop"
          type="is-light is-right"
          :delay="1000">
          <button class="button is-dark" @click="rewinder(exp, run)">
            <b-icon icon="history" />
          </button>
        </b-tooltip>
      </div>
      <div class="column has-text-centered">
        <span style="font-weight: bold; font-size: x-large"
          >Experiment: {{ runName() }}</span
        >
      </div>
      <div class="column is-2">
        <div class="run-status">
          <span class="tag is-medium" :class="statusDecorator()">
            {{ status }}
          </span>
          <b-tooltip :label="statusLabel()" type="is-light is-left">
            <button
              class="button is-dark"
              :aria-label="statusLabel()"
              @click="controller(exp, run)">
              <b-icon :icon="running ? 'stop' : 'play'" />
            </button>
          </b-tooltip>
        </div>
      </div>
    </div>
    <div
      style="margin-top: 10px; border: 2px solid whitesmoke; background: #333">
      <vue-pipeline :ref="runRef()" :pipeline="nodes" @select="viewer" />
    </div>
  </div>
</template>

<script>
  import VuePipeline from '@/components/pipeline/Pipeline.vue';

  export default {
    components: {
      'vue-pipeline': VuePipeline,
    },

    props: {
      exp: {
        type: String,
      },
      run: {
        type: Number,
        default: 0,
      },
      name: {
        type: String,
      },
      loop: {
        type: Number,
        default: 0,
      },
      running: {
        type: Boolean,
        default: false,
      },
      nodes: {
        type: Array,
        default: () => [],
      },
      viewer: {
        type: Function,
      },
      controller: {
        type: Function,
      },
      rewinder: {
        type: Function,
      },
    },

    computed: {
      status() {
        return this.running ? 'running' : 'stopped';
      },
    },

    methods: {
      runName() {
        let name = this.run;

        if (this.name) {
          name = this.name;
        }

        if (this.loop == 0) {
          return `${this.exp} - Run ${name}`;
        }

        return `${this.exp} - Run ${name} (loop ${this.loop})`;
      },

      runRef() {
        return 'pipeline-' + this.run + '-' + this.loop;
      },

      statusLabel() {
        return this.running ? 'stop scorch run' : 'start scorch run';
      },

      statusDecorator() {
        return this.running ? 'is-success' : 'is-danger';
      },
    },
  };
</script>

<style scoped>
  .run-status {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 0.5rem;
  }
</style>
