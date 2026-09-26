<template>
  <div class="content">
    <!-- UPLOAD MODAL -->
    <b-modal v-model="uploader.active" has-modal-card>
      <div class="modal-card" style="width: auto">
        <header class="modal-card-head">
          <p class="modal-card-title">Upload a Disk</p>
        </header>
        <section class="modal-card-body">
          <b-field v-if="currentUploadProgress == null">
            <b-upload
              :model-value="null"
              drag-drop
              :accept="uploadAccept"
              @update:modelValue="uploadDisk">
              <section class="section">
                <div class="content has-text-centered">
                  <p>
                    <b-icon icon="upload" size="is-large"></b-icon>
                  </p>
                  <p>Drop your disk here or click to upload</p>
                  <p>(Valid file types are {{ uploadTypesText }})</p>
                </div>
              </section>
            </b-upload>
          </b-field>
          <template v-else>
            <p>Uploading {{ uploader.name }}</p>
            <b-progress
              :value="currentUploadProgress"
              show-value
              format="percent"
              type="is-success"
              size="is-medium" />
            <p class="is-size-7">
              Closing this window does not stop the upload; the upload button
              shows its progress.
            </p>
          </template>
        </section>
      </div>
    </b-modal>

    <!-- DETAILS MODAL -->
    <b-modal
      v-model="detailsModal.active"
      @close="() => (detailsModal.active = false)"
      has-modal-card>
      <div class="modal-card">
        <header class="modal-card-head">
          <p class="modal-card-title">{{ detailsModal.disk.name }}</p>
        </header>
        <section class="modal-card-body">
          <p class="title is-5">Details</p>
          <dl>
            <div>
              <dt>Full Path:</dt>
              <dd>{{ detailsModal.disk.fullPath }}</dd>
            </div>
            <div>
              <dt>Kind:</dt>
              <dd>{{ detailsModal.disk.kind }}</dd>
            </div>
            <div>
              <dt>Size on Disk:</dt>
              <dd>{{ detailsModal.disk.size }}</dd>
            </div>
            <div>
              <dt>Virtual Size:</dt>
              <dd>{{ detailsModal.disk.virtualSize }}</dd>
            </div>
            <div>
              <dt>Experiment:</dt>
              <dd>{{ detailsModal.disk.experiment || 'N/A' }}</dd>
            </div>
            <div>
              <dt>In Use:</dt>
              <dd>{{ detailsModal.disk.inUse }}</dd>
            </div>
          </dl>
          <div
            v-if="
              detailsModal.disk.backingImages &&
              detailsModal.disk.backingImages.length > 0
            ">
            <hr />
            <p class="title is-5">Backing Chain</p>
            <div style="text-align: center">
              <b>{{ detailsModal.disk.name }}</b>
              <div
                v-for="(i, index) in detailsModal.disk.backingImages"
                :key="index">
                &darr;<br />
                <a
                  @click="detailsModal.disk = disks.find((d) => d.name == i)"
                  >{{ i }}</a
                >
              </div>
            </div>
          </div>

          <div class="actions">
            <hr />
            <p class="title is-5">Actions</p>
            <b-button
              type="is-text"
              expanded
              @click="snapshotDisk(detailsModal.disk.fullPath)"
              :disabled="shouldDisableAction('snapshot')">
              <b>Snapshot</b> - Creates a new image backed by this image
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="() => (commitModal.active = true)"
              :disabled="shouldDisableAction('commit')">
              <b>Commit</b> - Commits change in this image to its backing image
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="() => (rebaseModal.active = true)"
              :disabled="shouldDisableAction('rebase')">
              <b>Rebase</b> - Updates image and rebases onto a different backing
              image
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="cloneDisk(detailsModal.disk.fullPath)"
              :disabled="shouldDisableAction('clone')">
              <b>Clone</b> - Creates a copy of the disk file
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="resizeDisk(detailsModal.disk.fullPath)"
              :disabled="shouldDisableAction('resize')">
              <b>Resize</b>
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="downloadDisk(detailsModal.disk.fullPath)"
              :disabled="shouldDisableAction('download')">
              <b>Download</b>
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="renameDisk(detailsModal.disk.fullPath)"
              :disabled="shouldDisableAction('rename')">
              <b>Rename</b>
            </b-button>
            <hr class="action-separator" />
            <b-button
              type="is-text"
              expanded
              @click="deleteDisk(detailsModal.disk.fullPath)"
              :disabled="shouldDisableAction('delete')">
              <b>Delete</b>
            </b-button>
          </div>
        </section>
      </div>
    </b-modal>
    <!-- REBASE MODAL -->
    <b-modal v-model="rebaseModal.active" :can-cancel="false" has-modal-card>
      <div class="modal-card" style="max-width: 460px">
        <section class="modal-card-body">
          Are you sure you want to rebase this image onto a different backing
          image?<br />
          By default changes between the old and new backing images will be
          written to this image. Selecting "None" for the backing image will
          cause the image to become independent.<br />
          Selecting "Change Reference Only" will only change the backing image
          name without updating files.
          <b-select
            placeholder="New Backing Image"
            v-model="rebaseModal.dst"
            style="margin-bottom: 8px; margin-top: 16px">
            <option value="">None</option>
            <template v-for="d in disks" :key="d.fullPath">
              <option v-if="d !== detailsModal.disk" :value="d.fullPath">
                {{ d.name }}
              </option>
            </template>
          </b-select>
          <b-checkbox v-model="rebaseModal.unsafe"
            >Change reference only</b-checkbox
          >
        </section>
        <footer class="modal-card-foot" style="justify-content: flex-end">
          <b-button
            label="Cancel"
            @click="() => (rebaseModal.active = false)"
            :disabled="rebaseModal.isWaiting" />
          <b-button
            label="OK"
            type="is-primary"
            :loading="rebaseModal.isWaiting"
            @click="
              () =>
                rebaseDisk(
                  detailsModal.disk.fullPath,
                  rebaseModal.dst,
                  rebaseModal.unsafe,
                )
            " />
        </footer>
      </div>
    </b-modal>
    <!-- COMMIT MODAL -->
    <b-modal v-model="commitModal.active" :can-cancel="false" has-modal-card>
      <div class="modal-card" style="max-width: 460px">
        <section class="modal-card-body">
          Are you sure you want to commit the changes in this disk to its
          parent?<br />
          By default this disk is left unchanged, but you may select to delete
          it if it's no longer needed.
          <b-field style="margin-top: 16px">
            <b-checkbox v-model="commitModal.delete"
              >Delete this disk after commit</b-checkbox
            >
          </b-field>
        </section>
        <footer class="modal-card-foot" style="justify-content: flex-end">
          <b-button
            label="Cancel"
            @click="() => (commitModal.active = false)"
            :disabled="commitModal.isWaiting" />
          <b-button
            label="OK"
            type="is-primary"
            :loading="commitModal.isWaiting"
            @click="
              () => commitDisk(detailsModal.disk.fullPath, commitModal.delete)
            " />
        </footer>
      </div>
    </b-modal>
    <!-- CONTENT -->
    <b-field grouped position="is-right" style="margin: 12px 0px">
      <div
        v-if="paginationNeeded"
        class="control is-flex is-align-items-center">
        <b-switch v-model="table.isPaginated" size="is-small" type="is-light"
          >Paginate</b-switch
        >
      </div>
      <b-field>
        <b-autocomplete
          v-model="filterString"
          placeholder="Find a disk"
          icon="search"
          @select="(option) => (selected = option)"
          :data="filteredDisks.map((d) => d.name)"
          style="width: 512px">
        </b-autocomplete>

        <p class="control">
          <button class="button input-button" @click="filterString = ''">
            <b-icon icon="window-close"></b-icon>
          </button>
        </p>
      </b-field>
      <b-tooltip
        v-if="roleAllowed('disks', 'upload')"
        :label="
          currentUploadProgress == null ? 'Upload a disk' : 'Upload progress'
        "
        type="is-light is-left">
        <button
          class="button is-light"
          style="margin-left: 8px"
          aria-label="Upload a disk"
          @click="uploader.active = true">
          <b-icon v-if="currentUploadProgress == null" icon="upload"></b-icon>
          <span v-else style="width: 32px">{{ currentUploadProgress }}%</span>
        </button>
      </b-tooltip>
    </b-field>

    <b-table
      :data="filteredDisks"
      @click="rowClick"
      :row-class="(r, i) => 'is-clickable'"
      :paginated="table.isPaginated && paginationNeeded"
      :per-page="table.perPage"
      v-model:current-page="table.currentPage"
      :pagination-simple="table.isPaginationSimple"
      :pagination-size="table.paginationSize"
      :default-sort-direction="table.defaultSortDirection"
      sortable
      hoverable
      default-sort="name">
      <template #empty>
        <section class="section">
          <div class="content has-text-white has-text-centered">
            {{ emptyText }}
          </div>
        </section>
      </template>

      <b-table-column field="name" label="Name" sortable v-slot="props">
        {{ props.row.name }}
      </b-table-column>

      <b-table-column field="kind" label="Kind" sortable v-slot="props">
        {{ props.row.kind }}
      </b-table-column>

      <b-table-column
        field="inUse"
        label="In Use"
        centered
        sortable
        v-slot="props">
        <b-icon v-if="props.row.inUse" icon="play-circle" size="is-small" />
      </b-table-column>

      <b-table-column
        field="size"
        label="Size"
        sortable
        :custom-sort="sortBySize"
        v-slot="props">
        {{ props.row.size }}
      </b-table-column>
    </b-table>
  </div>
</template>

<script>
  import axiosInstance from '@/utils/axios.js';
  import { showError, useErrorNotification } from '@/utils/errorNotif';
  import { usePhenixStore } from '@/store.js';
  import { useTable } from '@/utils/useTable.js';
  import { roleAllowed } from '@/utils/rbac.js';
  import { createPageLoader, loadingText } from '@/utils/pageLoader.js';
  import { pageFetchers } from '@/utils/pageData.js';
  import { addWsHandler, removeWsHandler } from '@/utils/websocket';

  // the disk kinds phenix recognizes, see knownImageExtensions in
  // src/go/api/disk/details.go
  const UPLOAD_TYPES = ['.qcow2', '.qc2', '_rootfs.tgz', '.hdd', '.iso'];

  export default {
    setup() {
      return { ...useTable({ name: 'disks' }), roleAllowed };
    },
    async created() {
      this.rescan = false;
      this.loader = createPageLoader({
        key: 'disks',
        fetch: (signal) => pageFetchers.disks(signal, { rescan: this.rescan }),
        apply: (disks) => {
          this.disks = disks;
          this.loaded = true;
        },
        // the header button has the server inspect every image again, not
        // just the ones that changed
        refresh: async () => {
          this.rescan = true;
          try {
            return await this.loader.load();
          } finally {
            this.rescan = false;
          }
        },
      });
      this.loader.start();
      addWsHandler(this.handleWs);
    },

    beforeUnmount() {
      removeWsHandler(this.handleWs);
      this.loader.stop();
    },

    computed: {
      // file pickers match extensions only, so rootfs archives show as .tgz
      uploadAccept() {
        return UPLOAD_TYPES.map((t) => t.replace('_rootfs', '')).join(',');
      },
      uploadTypesText() {
        const types = UPLOAD_TYPES.map((t) =>
          t.startsWith('.') ? t : `*${t}`,
        );
        return `${types.slice(0, -1).join(', ')}, and ${types.at(-1)}`;
      },
      emptyText() {
        if (!this.loaded) return loadingText('disks');
        if (this.disks.length === 0) return 'No disk images found';
        return 'No disk images match your search';
      },
      paginationNeeded() {
        return this.disks.length > this.table.perPage;
      },
      filteredDisks() {
        return this.disks == null
          ? []
          : this.disks.filter(
              (disk) =>
                disk.name
                  .toLowerCase()
                  .indexOf(this.filterString.toLowerCase()) >= 0,
            );
      },
    },

    methods: {
      resetData() {
        this.detailsModal.active = false;
        this.rebaseModal = {
          active: false,
          unsafe: false,
          isWaiting: false,
          dst: '',
        };
        this.commitModal = {
          active: false,
          delete: false,
          isWaiting: false,
        };
      },
      handleWs(msg) {
        // the server saw images added, changed or removed
        if (msg.resource.type === 'disks' && msg.resource.action === 'update') {
          this.loader.load();
        }
      },
      // closes any open dialog and reloads the list after a change
      updateDisks() {
        this.resetData();
        this.loader.load();
      },
      rowClick(row) {
        this.detailsModal.disk = row;
        this.detailsModal.active = true;
      },
      shouldDisableAction(action) {
        let disk = this.detailsModal.disk;
        switch (action) {
          case 'snapshot':
            return (
              disk.inUse || disk.kind != 'VM' || !roleAllowed('disks', 'create')
            );
          case 'commit':
            return (
              disk.inUse ||
              (disk.backingImages && disk.backingImages.length == 0) ||
              disk.kind != 'VM'
            );
          case 'rebase':
            return (
              disk.inUse ||
              disk.kind != 'VM' ||
              !roleAllowed('disks', 'update', disk.name)
            );
          case 'delete':
            return disk.inUse || !roleAllowed('disks', 'delete', disk.name);
          case 'rename':
          case 'resize':
            return disk.inUse || !roleAllowed('disks', 'update', disk.name);
          case 'clone':
            return !roleAllowed('disks', 'create');
          case 'download':
            return !roleAllowed('disks', 'get', disk.name);
          default:
            return false;
        }
      },
      actionWrapper(httpPath, dialog = null, method = 'post') {
        if (dialog != null) {
          dialog.startLoading();
        }

        axiosInstance({
          method: method,
          url: httpPath,
        })
          .then(() => {
            this.updateDisks();
            if (dialog != null) {
              dialog.close();
            }
          })
          .catch((err) => {
            useErrorNotification(err);
            if (dialog != null) {
              dialog.cancelLoading();
            }
          });
      },
      commitDisk(path, deleteOnSuccess) {
        this.commitModal.isWaiting = true;
        axiosInstance
          .post(`disks/commit?disk=${path}`)
          .then(() => {
            if (deleteOnSuccess) {
              this.actionWrapper(`disks?disk=${path}`, null, 'delete');
            } else {
              this.updateDisks();
            }
          })
          .catch((err) => {
            useErrorNotification(err);
          });
      },
      snapshotDisk(path) {
        this.$buefy.dialog.prompt({
          message:
            'Are you sure you want to snapshot this disk? This will create a new disk backed by this image.',
          inputAttrs: {
            type: 'text',
            placeholder: 'New image name',
          },
          canCancel: ['button'],
          closeOnConfirm: false,
          onConfirm: (value, dialog) =>
            this.actionWrapper(
              `disks/snapshot?disk=${path}&new=${value}`,
              dialog,
            ),
        });
      },
      rebaseDisk(path, dst, unsafe) {
        this.rebaseModal.isWaiting = true;
        this.actionWrapper(
          `disks/rebase?disk=${path}&backing=${dst}&unsafe=${unsafe}`,
        );
      },
      resizeDisk(path) {
        this.$buefy.dialog.prompt({
          message:
            'Are you sure you want to resize this disk? The size must end with one of "K,M,G,T,P,E" and may be relative by prefixing with +/- (e.g., "50G" or "-512M").<br>Resizing must be accompanied by VM OS changes to either expand partitions after resizing or shrink partitions beforehand. <b class="has-text-danger">Data loss will occur if size is reduced without modifying the OS first.</b>',
          inputAttrs: {
            type: 'text',
            placeholder: 'New size',
            pattern: '[+-]?\\d+[KMGTPE]',
          },
          canCancel: ['button'],
          closeOnConfirm: false,
          onConfirm: (value, dialog) =>
            this.actionWrapper(
              `disks/resize?disk=${path}&size=${encodeURIComponent(value)}`,
              dialog,
            ),
        });
      },
      cloneDisk(path) {
        this.$buefy.dialog.prompt({
          message: 'Are you sure you want to clone this disk?',
          inputAttrs: {
            type: 'text',
            placeholder: 'New image name',
            value: path.split('/').pop(),
          },
          canCancel: ['button'],
          closeOnConfirm: false,
          onConfirm: (value, dialog) =>
            this.actionWrapper(`disks/clone?disk=${path}&new=${value}`, dialog),
        });
      },
      renameDisk(path) {
        this.$buefy.dialog.prompt({
          message:
            'Are you sure you want to rename this disk? <b class="has-text-danger">If this disk backs others, they must be rebased to use the new name.</b>',
          inputAttrs: {
            type: 'text',
            placeholder: 'New name',
            value: path.split('/').pop(),
          },
          canCancel: ['button'],
          closeOnConfirm: false,
          onConfirm: (value, dialog) =>
            this.actionWrapper(
              `disks/rename?disk=${path}&new=${value}`,
              dialog,
            ),
        });
      },
      deleteDisk(path) {
        this.$buefy.dialog.confirm({
          message:
            'Are you sure you want to delete this disk? <b class="has-text-danger">If this disk backs others, they will become invalid.</b>',
          canCancel: ['button'],
          closeOnConfirm: false,
          onConfirm: (_, dialog) =>
            this.actionWrapper(`disks?disk=${path}`, dialog, 'delete'),
        });
      },
      downloadDisk(path) {
        this.$buefy.dialog.confirm({
          message: 'Are you sure you want to download this disk?',
          onConfirm: () => {
            const store = usePhenixStore();
            const basePath = import.meta.env.BASE_URL;
            window.open(
              `${basePath}api/v1/disks/download?token=${store.token}&disk=${encodeURIComponent(path)}`,
              '_blank',
            );
          },
        });
      },
      uploadDisk(file) {
        if (!file) return;
        if (!UPLOAD_TYPES.some((ext) => file.name.endsWith(ext))) {
          showError(
            `Cannot upload ${file.name}`,
            `Valid disk file types are ${this.uploadTypesText}.`,
          );
          return;
        }

        let formData = new FormData();
        formData.append('file', file);
        this.uploader.name = file.name;
        this.currentUploadProgress = 0;
        axiosInstance
          .post(`disks`, formData, {
            headers: { 'Content-Type': 'multipart/form-data' },
            onUploadProgress: (event) => {
              this.currentUploadProgress = Math.round(
                (event.loaded / event.total) * 100,
              );
            },
          })
          .then(() => {
            this.currentUploadProgress = null;
            this.uploader.active = false;
            this.updateDisks();
          })
          .catch((err) => {
            useErrorNotification(
              `Error uploading: ${err.response?.data ?? err.message}`,
            );
            this.currentUploadProgress = null;
          });
      },
      // converts a human-readable string in IEC format to a byte count
      toByteCount(s) {
        const units = 'KMGTPE';
        const base = s.match(/[/.0-9]*/);
        const unit = s[s.indexOf(' ') + 1];
        if (unit == 'B') {
          return parseFloat(base);
        }
        return parseFloat(base) * Math.pow(1024, units.indexOf(unit));
      },
      sortBySize(diskA, diskB, isAsc) {
        return (
          (this.toByteCount(diskA.size) - this.toByteCount(diskB.size)) *
          (isAsc ? 1 : -1)
        );
      },
    },

    data() {
      return {
        currentUploadProgress: null,
        uploader: { active: false, name: null },
        disks: [],
        filterString: '',
        loaded: false, // false until the first list arrives
        detailsModal: {
          active: false,
          disk: {},
        },
        rebaseModal: {
          active: false,
          isWaiting: false,
          unsafe: false,
          dst: '',
        },
        commitModal: {
          active: false,
          isWaiting: false,
          delete: false,
        },
      };
    },
  };
</script>

<style scoped>
  .b-tooltip:after {
    white-space: pre !important;
  }

  dl {
    display: table;
  }

  dl > div {
    display: table-row;
  }

  dl > div > dt,
  dl > div > dd {
    display: table-cell;
    padding: 0.25em;
  }

  dl > div > dt {
    font-weight: bold;
    width: 20%;
  }

  hr {
    margin: 4px 0px;
  }

  .action-button {
    color: dimgray;
    padding: 8px;
    cursor: pointer !important;
  }

  .action-button:hover {
    background-color: #ddd;
  }

  .action-separator {
    margin: 0 8px;
  }

  .actions > button {
    text-align: start;
    /* color: blue; */
    text-decoration: none;
    display: inline;
  }

  .file-cta,
  .file-cta > p,
  .file-cta:hover {
    border: none;
    background-color: #686868;
    color: whitesmoke !important;
  }
</style>
