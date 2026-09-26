<template>
  <b-modal
    v-model="isUploaderModalActive"
    @close="resetUploader"
    has-modal-card>
    <div class="modal-card" style="width: auto">
      <header class="modal-card-head x-modal-dark">
        <p class="modal-card-title">Upload a Config</p>
      </header>
      <section class="modal-card-body x-modal-dark">
        <b-field>
          <b-upload
            v-model="uploaderFile"
            drag-drop
            @update:modelValue="uploadFile">
            <section class="section">
              <div class="content has-text-centered">
                <p>
                  <b-icon icon="upload" size="is-large"></b-icon>
                </p>
                <p>Drop your config here or click to upload</p>
                <p>(Valid file types are .yaml, .yml, and .json)</p>
              </div>
            </section>
          </b-upload>
        </b-field>
      </section>
    </div>
  </b-modal>
  <b-modal v-model="viewer.isActive" @close="resetViewer" has-modal-card>
    <div class="modal-card" style="width: 50em">
      <header class="modal-card-head x-modal-dark">
        <p class="modal-card-title x-config-text">{{ viewer.title }}</p>
      </header>
      <section class="modal-card-body x-modal-dark">
        <div class="control">
          <textarea
            class="textarea x-config-text has-fixed-size"
            rows="30"
            :value="viewer.obj ?? 'Loading…'"
            readonly />
        </div>
      </section>
      <footer class="modal-card-foot x-modal-dark buttons is-right">
        <button
          v-if="roleAllowed('configs', 'update', viewer.config.metadata.name)"
          class="button is-success"
          @click="$emit('edit', viewer.config)">
          Edit Config
        </button>
        <button
          class="button is-info"
          :class="{ 'is-loading': isDownloading(viewer.config) }"
          @click="download([viewer.config])">
          <b-icon icon="download"></b-icon>
        </button>
        <button class="button is-dark" @click="resetViewer">Exit</button>
      </footer>
    </div>
  </b-modal>
  <!--header-->
  <div class="level">
    <div class="level-left" />
    <div class="level-right">
      <div class="level-item">
        <b-field position="is-right" grouped>
          <div
            v-if="paginationNeeded"
            class="control is-flex is-align-items-center">
            <b-switch v-model="isPaginated" size="is-small" type="is-light"
              >Paginate</b-switch
            >
          </div>
          <b-field
            v-if="
              selectedConfigs.length > 0 &&
              selectedConfigs.every((c) =>
                roleAllowed('configs', 'get', c.metadata.name),
              )
            ">
            <b-tooltip label="download selected configs" type="is-light is-top">
              <button
                class="button is-light action"
                @click="download(selectedConfigs)">
                <b-icon icon="download"></b-icon>
              </button>
            </b-tooltip>
          </b-field>
          <b-field
            v-if="
              selectedConfigs.length > 0 &&
              selectedConfigs.every((c) =>
                roleAllowed('configs', 'delete', c.metadata.name),
              )
            ">
            <b-tooltip label="delete selected configs" type="is-light is-top">
              <button
                class="button is-light action"
                @click="deleteConfigs(selectedConfigs)">
                <b-icon icon="trash"></b-icon>
              </button>
            </b-tooltip>
          </b-field>
          <b-field>
            <b-select placeholder="Filter on Kind" v-model="filterKind">
              <option :value="null">All kinds</option>
              <option
                v-for="(k, index) in filterOptions"
                :key="index"
                :value="k">
                {{ k }}
              </option>
            </b-select>
          </b-field>
          <b-field>
            <b-autocomplete
              v-model="searchQuery"
              placeholder="Find a Config"
              icon="search"
              :data="filteredConfigs.map((c) => c.metadata.name)"
              @select="(option) => (filtered = option)">
              <template #empty> No results found </template>
            </b-autocomplete>
            <p class="control">
              <b-tooltip
                label="resets search filter and filter on kind"
                type="is-light"
                multilined>
                <button
                  class="button input-button"
                  @click="
                    searchQuery = '';
                    filterKind = null;
                  ">
                  <b-icon icon="window-close"></b-icon>
                </button>
              </b-tooltip>
            </p>
          </b-field>
          <b-field v-if="roleAllowed('configs', 'create')">
            <b-tooltip label="create a new config" type="is-light is-top">
              <button
                class="button is-light"
                id="main"
                @mouseenter="loadEditor"
                @click="$emit('create')">
                <b-icon icon="plus"></b-icon>
              </button>
            </b-tooltip>
          </b-field>
          <b-field v-if="roleAllowed('configs', 'create')">
            <b-tooltip label="upload a new config" type="is-light is-top">
              <button
                class="button is-light"
                id="main"
                @click="isUploaderModalActive = true">
                <b-icon icon="upload"></b-icon>
              </button>
            </b-tooltip>
          </b-field>
        </b-field>
      </div>
    </div>
  </div>
  <!--table-->
  <div style="margin-top: -1em">
    <b-table
      :data="filteredConfigs"
      :paginated="isPaginated && paginationNeeded"
      per-page="10"
      pagination-simple="true"
      pagination-size="is-small"
      default-sort="kind"
      checkable
      v-model:checked-rows="selectedConfigs"
      :loading="isWaiting"
      ref="cfgTable">
      <template #empty>
        <section class="section">
          <div class="content has-text-white has-text-centered">
            {{ emptyText }}
          </div>
        </section>
      </template>

      <b-table-column
        field="kind"
        label="Kind"
        width="200"
        header-class="sort-inline"
        sortable
        v-slot="props">
        {{ props.row.kind }}
      </b-table-column>

      <b-table-column
        field="metadata.name"
        label="Name"
        width="400"
        header-class="sort-inline"
        sortable
        v-slot="props">
        <template v-if="roleAllowed('configs', 'get', props.row.metadata.name)">
          <b-tooltip label="view config" type="is-dark">
            <div class="field is-clickable">
              <div
                @mouseenter="prepareOpen(props.row)"
                @click="viewConfig(props.row)">
                {{ props.row.metadata.name }}
              </div>
            </div>
          </b-tooltip>
          &nbsp;
          <b-tag type="is-info" v-if="isBuilderTopology(props.row)"
            >builder</b-tag
          >
        </template>
        <template v-else>
          {{ props.row.metadata.name }}
          &nbsp;
          <b-tag type="is-info" v-if="isBuilderTopology(props.row)"
            >builder</b-tag
          >
        </template>
      </b-table-column>

      <b-table-column
        field="metadata.updated"
        label="Last Updated"
        header-class="sort-inline"
        sortable
        :custom-sort="sortByUpdated"
        v-slot="props">
        {{ props.row.metadata.updated }}
        <span v-if="props.row.metadata.updated" class="has-text-grey-lighter">
          ({{ relativeTime(props.row.metadata.updated, now) }})
        </span>
      </b-table-column>

      <b-table-column label="Actions" centered v-slot="props">
        <b-tooltip
          class="action"
          :delay="500"
          label="edit config file"
          type="is-light"
          multilined>
          <button
            v-if="roleAllowed('configs', 'update', props.row.metadata.name)"
            class="button is-light is-small action"
            @mouseenter="prepareOpen(props.row)"
            @click="$emit('edit', props.row)">
            <b-icon icon="edit"></b-icon>
          </button>
        </b-tooltip>
        <b-tooltip
          class="action"
          :delay="500"
          label="download config"
          type="is-light"
          multilined>
          <button
            v-if="roleAllowed('configs', 'get', props.row.metadata.name)"
            class="button is-light is-small action"
            :class="{ 'is-loading': isDownloading(props.row) }"
            @click="download([props.row])">
            <b-icon icon="download"></b-icon>
          </button>
        </b-tooltip>
        <b-tooltip
          class="action"
          :delay="500"
          label="delete config"
          type="is-light"
          multilined>
          <button
            v-if="roleAllowed('configs', 'delete', props.row.metadata.name)"
            class="button is-light is-small action"
            @click="deleteConfigs([props.row])">
            <b-icon icon="trash"></b-icon>
          </button>
        </b-tooltip>
      </b-table-column>
    </b-table>
  </div>
</template>

<script>
  import axiosInstance from '@/utils/axios.js';
  import YAML from 'js-yaml';

  import FileSaver from 'file-saver';
  import { roleAllowed } from '@/utils/rbac.js';
  import { showError, useErrorNotification } from '@/utils/errorNotif';
  import {
    configKey,
    forgetConfig,
    fullConfig,
    prefetchConfig,
  } from '@/utils/configCache.js';
  import { loadAce } from '@/utils/loadAce.js';
  import { relativeTime } from '@/utils/relativeTime.js';
  import { createPageLoader, loadingText } from '@/utils/pageLoader.js';
  import { pageFetchers } from '@/utils/pageData.js';
  import { loadPaginate, savePaginate } from '@/utils/paginatePref.js';

  export default {
    emits: ['edit', 'create'],
    setup() {
      return { roleAllowed };
    },
    watch: {
      isPaginated(on) {
        savePaginate('configs', on);
      },
    },

    data() {
      return {
        configs: [],
        loaded: false, // false until the first list arrives
        isWaiting: false, // set while a change is being saved

        //filters
        filterKind: null,
        searchQuery: '',
        filterOptions: [
          'Topology',
          'Scenario',
          'Experiment',
          'Image',
          'User',
          'Role',
        ],
        //table
        isPaginated: loadPaginate('configs'),
        perPage: 10,
        currentPage: 1,
        selectedConfigs: [],

        //uploader modal
        isUploaderModalActive: false,
        uploaderFile: null,

        viewer: {
          isActive: false,
          config: { kind: null, metadata: { name: null } },
          title: null,
          obj: null,
        },

        downloading: new Set(), // configs being downloaded, by key
        now: Date.now(), // for the Last Updated column's relative times
      };
    },
    created() {
      // keeps the relative times current
      this.clock = setInterval(() => (this.now = Date.now()), 30000);
      this.loader = createPageLoader({
        key: 'configs',
        fetch: pageFetchers.configs,
        apply: (configs) => {
          this.configs = configs;
          this.loaded = true;
        },
      });
      this.loader.start();
    },
    beforeUnmount() {
      this.loader.stop();
      clearInterval(this.clock);
    },
    computed: {
      emptyText() {
        if (!this.loaded) return loadingText('configs');
        if (this.configs.length === 0) return 'No configs found';
        return 'No configs match your search';
      },
      paginationNeeded() {
        return this.filteredConfigs.length > 10;
      },
      filteredConfigs() {
        // a plain substring match: the search box is not a regular expression
        const search = this.searchQuery.toLowerCase();
        return this.configs.filter(
          (cfg) =>
            (!this.filterKind || cfg.kind == this.filterKind) &&
            cfg.metadata.name.toLowerCase().includes(search),
        );
      },
    },
    methods: {
      relativeTime,

      // Starts loading what opening a config needs once the pointer is on its
      // way: the config itself and, for the editor, Ace. Ace is not prefetched
      // with the pages since it would add ~150 kB to every first visit.
      prepareOpen(cfg) {
        prefetchConfig(cfg);
        this.loadEditor();
      },
      loadEditor() {
        // the editor reports a failure when opened
        loadAce().catch(() => {});
      },

      sortByUpdated(a, b, isAsc) {
        const diff =
          Date.parse(a.metadata.updated ?? 0) -
          Date.parse(b.metadata.updated ?? 0);
        return isAsc ? diff : -diff;
      },

      isDownloading(cfg) {
        return cfg.kind !== null && this.downloading.has(configKey(cfg));
      },

      updateConfigs() {
        this.loader.load();
      },
      isBuilderTopology(cfg) {
        if (cfg.kind == 'Topology') {
          if ('annotations' in cfg.metadata) {
            return 'builder-xml' in cfg.metadata.annotations;
          }
        }

        return false;
      },
      download(configList) {
        const configs = configList.map(configKey);
        // the button spins until the file is ready
        configs.forEach((key) => this.downloading.add(key));
        axiosInstance
          .post('configs/download', JSON.stringify(configs), {
            headers: {
              'Content-Type': 'application/json',
              Accept: 'application/x-yaml',
            },
            responseType: 'blob',
          })
          .then((response) => {
            if (configs.length == 1) {
              let body = new Blob([response.data], {
                type: 'text/plain',
              });
              const fileName = configs[0].replace('/', '-') + '.yml';
              FileSaver.saveAs(body, fileName);
            } else {
              FileSaver.saveAs(response.data, 'configs.zip');
            }
          })
          .catch((err) => {
            useErrorNotification(err);
          })
          .finally(() => {
            configs.forEach((key) => this.downloading.delete(key));
          });
      },
      deleteConfigs(configList) {
        const configs = configList.map(
          (conf) => `${conf.kind}/${conf.metadata.name}`,
        );
        let msg;
        if (configs.length > 1) {
          msg =
            'This will delete ' +
            configs.length +
            ' configs. Are you sure you want to do this?';
        } else {
          msg =
            'This will delete the ' +
            configs[0] +
            ' config. Are you sure you want to do this?';
        }
        this.$buefy.dialog.confirm({
          title: 'Delete the Config',
          message: msg,
          cancelText: 'Cancel',
          confirmText: 'Delete',
          type: 'is-danger',
          hasIcon: true,
          onConfirm: () => {
            for (var i = 0; i < configs.length; i++) {
              this.isWaiting = true;
              axiosInstance
                .delete('configs/' + configs[i])
                .then(() => {
                  //delete from config list
                  let configsSet = new Set(configs);
                  configList.forEach(forgetConfig);
                  this.configs = this.configs.filter((item) => {
                    const key = `${item.kind}/${item.metadata.name}`;
                    return !configsSet.has(key);
                  });

                  let confirmMsg;
                  if (configs.length > 1) {
                    confirmMsg = 'The configs have been deleted.';
                  } else {
                    confirmMsg =
                      'The ' + configs[0] + ' config has been deleted.';
                  }

                  this.isWaiting = false;

                  this.$buefy.toast.open({
                    message: confirmMsg,
                    type: 'is-success',
                    duration: 4000,
                  });
                })
                .catch((err) => {
                  useErrorNotification(err);
                  this.isWaiting = false;
                });
            }
          },
        });
      },

      uploadFile(file) {
        let ext = /\.yaml|\.yml|\.json$/i;

        if (!ext.exec(file.name)) {
          this.$buefy.toast.open({
            message: 'Valid file types are .yaml, .yml, and .json',
            type: 'is-danger',
            duration: 4000,
          });
          return;
        }

        let formData = new FormData();
        formData.append('fileupload', file);

        axiosInstance
          .post('configs', formData)
          .then(() => {
            this.$buefy.toast.open({
              message: 'The file ' + file.name + ' was uploaded',
              type: 'is-success',
              duration: 4000,
            });
            this.updateConfigs();
          })
          .catch((err) => {
            const validation = err.response?.data?.metadata?.validation;
            if (validation) {
              showError('Validation Error', validation);
            } else {
              useErrorNotification(err);
            }
          });
        this.resetUploader();
        this.isWaiting = false;
      },
      resetUploader() {
        this.isUploaderModalActive = false;
        this.uploaderFile = null;
      },
      resetViewer() {
        this.viewer.isActive = false;
        this.viewer.config = { kind: null, metadata: { name: null } };
        this.viewer.title = null;
        this.viewer.obj = null;
      },
      // Opens the viewer at once and fills it when the config arrives; the
      // editor reuses the loaded config.
      async viewConfig(cfg) {
        this.viewer.config = cfg;
        this.viewer.title = configKey(cfg);
        this.viewer.obj = null;
        this.viewer.isActive = true;
        this.loadEditor();

        try {
          const obj = await fullConfig(cfg);
          // the viewer moved on to another config or closed meanwhile
          if (this.viewer.config !== cfg) return;

          // the builder's diagram is long and unreadable here
          if (obj.metadata.annotations?.['builder-xml']) {
            obj.metadata.annotations['builder-xml'] = '<SNIPPED>';
          }

          this.viewer.obj = YAML.dump(obj);
        } catch (err) {
          useErrorNotification(err);
          if (this.viewer.config === cfg) this.resetViewer();
        }
      },
    },
  };
</script>
<style scoped>
  .x-modal-dark :deep(textarea) {
    background-color: #686868;
    color: whitesmoke;
  }
  textarea {
    color: whitesmoke;
  }
</style>
