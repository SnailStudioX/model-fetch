<script>
  import { ui } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import JobCard from "./JobCard.svelte";
  import EmptyState from "./EmptyState.svelte";

  const tr = (key) => t(key, ui.lang);
  const jobs = $derived([...ui.jobs.values()].sort((a, b) => {
    const left = Date.parse(a.createdAt) || 0;
    const right = Date.parse(b.createdAt) || 0;
    if (left !== right) return right - left;
    return String(b.id).localeCompare(String(a.id));
  }));
</script>

{#if jobs.length === 0 && ui.jobsLoadFailed}
  <EmptyState title="common.error" hint="jobs.loadFailed" icon="activity" />
{:else if jobs.length === 0}
  <EmptyState title="jobs.emptyTitle" hint="jobs.emptyHint" icon="activity" />
{:else}
  {#each jobs as job (job.id)}
    <JobCard {job} />
  {/each}
{/if}
