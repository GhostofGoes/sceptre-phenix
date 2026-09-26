<!--
This component will display an experiment available to the user;
it will include a specific component rendering based on whether
the experiment is running or not. If the user is VM Viewer role,
this will only show a list of VMs that a user can view.
 -->

<template>
  <component v-if="component" :is="component" @running="show"></component>
  <section v-else class="section">
    <div class="content has-text-white has-text-centered">
      {{ failed ? 'Could not load the experiment' : 'Loading experiment…' }}
    </div>
  </section>
</template>

<script setup>
  import { usePhenixStore } from '@/store';
  import axiosInstance from '@/utils/axios';
  import { useErrorNotification } from '@/utils/errorNotif.js';
  import { cachedPage, fetchIntoCache } from '@/utils/pageCache.js';
  import { experimentKey, stoppedExperimentKey } from '@/utils/pageData.js';
  import { defineAsyncComponent, onBeforeUnmount, ref, shallowRef } from 'vue';
  import { useRoute } from 'vue-router';

  const route = useRoute();
  const component = shallowRef(null);
  const failed = ref(false);
  let running = null;

  // Shows the running or stopped view. The pages report the experiment's
  // state when their data arrives, so a stale guess is corrected.
  function show(isRunning) {
    if (isRunning === running) return;
    running = isRunning;

    let load;
    if (!isRunning) {
      load = () => import('./StoppedExperiment.vue');
    } else if (usePhenixStore().role.name === 'VM Viewer') {
      load = () => import('./VMtilesView.vue');
    } else {
      load = () => import('./RunningExperiment.vue');
    }
    component.value = defineAsyncComponent(load);
  }

  // Whether the experiment is running, from the link that opened it or from
  // data already loaded, so the page opens without waiting on the server.
  function knownRunning(name) {
    const fromLink = window.history.state?.running;
    if (fromLink !== undefined) return fromLink;

    const cached = cachedPage(experimentKey(name))?.data;
    if (cached) return cached.running;
    if (cachedPage(stoppedExperimentKey(name))) return false;

    const listed = cachedPage('experiments')?.data?.find(
      (exp) => exp.name === name,
    );
    return listed?.running;
  }

  const name = route.params.id;
  const known = knownRunning(name);
  let request = null;

  if (known !== undefined) {
    show(known);
  } else {
    // the running view makes the same request, so it joins this one
    request = fetchIntoCache(experimentKey(name), async (signal) => {
      const resp = await axiosInstance.get('experiments/' + name, { signal });
      return resp.data;
    });
    request.promise.then(
      (exp) => show(exp.running),
      (err) => {
        failed.value = true;
        useErrorNotification(err);
      },
    );
  }

  onBeforeUnmount(() => request?.release());
</script>
