<template>
  <div class="content">
    <b-modal v-model="isRoleModalActive" @close="resetRoleForm" has-modal-card>
      <div class="modal-card role-modal">
        <header class="modal-card-head">
          <p class="modal-card-title">{{ modalTitle }}</p>
        </header>
        <section class="modal-card-body">
          <b-field label="Role Name">
            <b-input v-model="roleForm.name" autofocus></b-input>
          </b-field>
          <b-field v-if="modalMode == 'create'" label="Metadata Name">
            <b-input v-model="roleForm.metadata_name"></b-input>
          </b-field>
          <p class="help" v-if="modalMode == 'create'">
            Leave blank to derive one from the role name.
          </p>

          <hr />
          <h4>Policies</h4>
          <div
            v-for="(policy, index) in roleForm.policies"
            :key="index"
            class="box">
            <div class="is-flex is-justify-content-space-between">
              <strong>Policy {{ index + 1 }}</strong>
              <button
                class="button is-small is-light"
                @click="removePolicy(index)"
                :disabled="roleForm.policies.length == 1">
                <b-icon icon="trash"></b-icon>
              </button>
            </div>
            <b-field label="Resources">
              <b-input
                v-model="policy.resourcesText"
                placeholder="experiments experiments/*"></b-input>
            </b-field>
            <b-field label="Resource Names">
              <b-input
                v-model="policy.resourceNamesText"
                placeholder="* */*"></b-input>
            </b-field>
            <b-field label="Verbs">
              <b-input
                v-model="policy.verbsText"
                placeholder="list get update"></b-input>
            </b-field>
          </div>
          <button class="button is-light" @click="addPolicy">
            <b-icon icon="plus"></b-icon>
            <span>Add Policy</span>
          </button>
        </section>
        <footer class="modal-card-foot buttons is-right">
          <button class="button is-light" @click="resetRoleForm">Cancel</button>
          <button
            class="button is-success"
            @click="saveRole"
            :disabled="!validRoleForm">
            Save Role
          </button>
        </footer>
      </div>
    </b-modal>

    <div class="is-flex is-justify-content-space-between mb-4">
      <h3>RBAC Roles</h3>
      <button
        v-if="roleAllowed('roles', 'create')"
        class="button is-light"
        @click="openCreateRole">
        <b-icon icon="plus"></b-icon>
      </button>
    </div>

    <b-table
      :data="roles"
      :loading="isLoadingRoles"
      detailed
      detail-key="metadata_name"
      default-sort="name">
      <b-table-column field="name" label="Role" sortable v-slot="props">
        {{ props.row.name }}
      </b-table-column>
      <b-table-column
        field="metadata_name"
        label="Metadata Name"
        sortable
        v-slot="props">
        {{ props.row.metadata_name }}
      </b-table-column>
      <b-table-column label="Policies" numeric v-slot="props">
        {{ props.row.policies.length }}
      </b-table-column>
      <b-table-column label="Actions" centered v-slot="props">
        <button
          v-if="roleAllowed('roles', 'update', props.row.metadata_name)"
          class="button is-small is-light"
          @click="openEditRole(props.row)">
          <b-icon icon="edit"></b-icon>
        </button>
        <button
          v-if="roleAllowed('roles', 'delete', props.row.metadata_name)"
          class="button is-small is-light"
          @click="confirmDeleteRole(props.row)">
          <b-icon icon="trash"></b-icon>
        </button>
      </b-table-column>

      <template #detail="props">
        <b-table :data="props.row.policies" narrowed>
          <b-table-column label="Resources" v-slot="policyProps">
            {{ joinValues(policyProps.row.resources) }}
          </b-table-column>
          <b-table-column label="Resource Names" v-slot="policyProps">
            {{ joinValues(policyProps.row.resourceNames) }}
          </b-table-column>
          <b-table-column label="Verbs" v-slot="policyProps">
            {{ joinValues(policyProps.row.verbs) }}
          </b-table-column>
        </b-table>
      </template>
    </b-table>

    <hr />
    <h3>Known Resources</h3>
    <b-table
      :data="resources"
      :loading="isLoadingResources"
      default-sort="resource">
      <b-table-column field="resource" label="Resource" sortable v-slot="props">
        {{ props.row.resource }}
      </b-table-column>
      <b-table-column label="Verbs" v-slot="props">
        {{ joinValues(props.row.verbs) }}
      </b-table-column>
    </b-table>
  </div>
</template>

<script>
  import axiosInstance from '@/utils/axios.js';
  import { useErrorNotification } from '@/utils/errorNotif';
  import { roleAllowed } from '@/utils/rbac.js';

  const emptyPolicy = () => ({
    resourcesText: '',
    resourceNamesText: '',
    verbsText: '',
  });

  export default {
    setup() {
      return { roleAllowed };
    },
    created() {
      this.refreshRoles();
      this.refreshResources();
    },
    data() {
      return {
        roles: [],
        resources: [],
        isLoadingRoles: false,
        isLoadingResources: false,
        isRoleModalActive: false,
        modalMode: 'create',
        roleForm: {
          metadata_name: '',
          name: '',
          policies: [emptyPolicy()],
        },
      };
    },
    computed: {
      modalTitle() {
        return this.modalMode == 'create'
          ? 'Create RBAC Role'
          : 'Edit RBAC Role';
      },
      validRoleForm() {
        return (
          this.roleForm.name.trim() != '' &&
          this.roleForm.policies.length > 0 &&
          this.roleForm.policies.every(
            (policy) =>
              policy.resourcesText.trim() != '' &&
              policy.verbsText.trim() != '',
          )
        );
      },
    },
    methods: {
      refreshRoles() {
        if (!roleAllowed('roles', 'list')) {
          return;
        }

        this.isLoadingRoles = true;
        axiosInstance
          .get('roles')
          .then((response) => {
            this.roles = response.data.roles;
          })
          .catch((err) => {
            useErrorNotification(err);
          })
          .finally(() => {
            this.isLoadingRoles = false;
          });
      },
      refreshResources() {
        if (!roleAllowed('roles/resources', 'list')) {
          return;
        }

        this.isLoadingResources = true;
        axiosInstance
          .get('roles/resources')
          .then((response) => {
            this.resources = response.data.resources;
          })
          .catch((err) => {
            useErrorNotification(err);
          })
          .finally(() => {
            this.isLoadingResources = false;
          });
      },
      openCreateRole() {
        this.modalMode = 'create';
        this.roleForm = {
          metadata_name: '',
          name: '',
          policies: [emptyPolicy()],
        };
        this.isRoleModalActive = true;
      },
      openEditRole(role) {
        this.modalMode = 'edit';
        this.roleForm = {
          metadata_name: role.metadata_name,
          name: role.name,
          policies: role.policies.map((policy) => ({
            resourcesText: this.joinValues(policy.resources),
            resourceNamesText: this.joinValues(policy.resourceNames),
            verbsText: this.joinValues(policy.verbs),
          })),
        };
        this.isRoleModalActive = true;
      },
      resetRoleForm() {
        this.isRoleModalActive = false;
        this.roleForm = {
          metadata_name: '',
          name: '',
          policies: [emptyPolicy()],
        };
      },
      addPolicy() {
        this.roleForm.policies.push(emptyPolicy());
      },
      removePolicy(index) {
        this.roleForm.policies.splice(index, 1);
      },
      saveRole() {
        const role = this.rolePayload();
        const request =
          this.modalMode == 'create'
            ? axiosInstance.post('roles', role)
            : axiosInstance.put(`roles/${this.roleForm.metadata_name}`, role);

        request
          .then(() => {
            this.$buefy.toast.open({
              message: 'Role saved',
              type: 'is-success',
              duration: 3000,
            });
            this.resetRoleForm();
            this.refreshRoles();
          })
          .catch((err) => {
            useErrorNotification(err);
          });
      },
      confirmDeleteRole(role) {
        this.$buefy.dialog.confirm({
          title: 'Delete RBAC Role',
          message:
            'This will permanently delete the ' +
            role.name +
            ' role. Are you sure?',
          cancelText: 'Cancel',
          confirmText: 'Delete',
          type: 'is-danger',
          hasIcon: true,
          onConfirm: () => this.deleteRole(role),
        });
      },
      deleteRole(role) {
        axiosInstance
          .delete(`roles/${role.metadata_name}`)
          .then(() => {
            this.$buefy.toast.open({
              message: 'Role deleted',
              type: 'is-success',
              duration: 3000,
            });
            this.refreshRoles();
          })
          .catch((err) => {
            useErrorNotification(err);
          });
      },
      rolePayload() {
        return {
          metadata_name: this.roleForm.metadata_name,
          name: this.roleForm.name,
          policies: this.roleForm.policies.map((policy) => ({
            resources: this.splitValues(policy.resourcesText),
            resourceNames: this.splitValues(policy.resourceNamesText),
            verbs: this.splitValues(policy.verbsText),
          })),
        };
      },
      splitValues(value) {
        return value
          .split(/\s+/)
          .map((entry) => entry.trim())
          .filter((entry) => entry != '');
      },
      joinValues(values) {
        return values ? values.join(' ') : '';
      },
    },
  };
</script>

<style scoped>
  .role-modal {
    width: 50em;
  }
</style>
