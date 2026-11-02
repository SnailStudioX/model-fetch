<script>
  import { ui, loadHistory, toast } from "../store.svelte.js";
  import { t } from "../i18n.js";
  import { formatBytes, formatRelativeDate, meaningfulHubDir } from "../format.js";
  import Icon from "../components/Icon.svelte";

  const tr = (key) => t(key, ui.lang);
  const entries = $derived([...ui.history.entries].reverse());

  // The history is what this page shows, so it is read on entering the page
  // rather than only after the user finds the refresh button — otherwise the
  // empty-state card is the first thing everyone sees. A failed read is recorded
  // on ui.history.loadFailed by loadHistory, so the card can say that instead.
  $effect(() => {
    loadHistory().catch(() => {});
  });
</script>

<header class="page-header">
  <div>
    <h1>{tr("history.title")}</h1>
    <p class="page-subtitle">{tr("history.subtitle")}</p>
  </div>
  <div class="page-actions">
    <button class="btn btn-secondary btn-sm" type="button" disabled={ui.history.busy} onclick={() => loadHistory().catch((err) => toast(err.message, "error"))}>
      <Icon name="refresh" size={14} /> {tr("common.refresh")}
    </button>
  </div>
</header>

<div id="historyList">
  {#if entries.length === 0}
    <div class="empty-state">
      <div class="empty-icon"><Icon name="clock" size={64} /></div>
      {#if ui.history.loadFailed}
        <h3>{tr("common.error")}</h3>
        <p>{tr("history.loadFailed")}</p>
      {:else}
        <h3>{tr("history.emptyTitle")}</h3>
        <p>{tr("history.emptyHint")}</p>
      {/if}
    </div>
  {:else}
    <div class="history-table-wrap">
      <table class="history-table">
        <thead>
          <tr>
            <th>{tr("common.colRepo")}</th><th>{tr("common.colStatus")}</th><th>{tr("common.colFiles")}</th><th>{tr("common.colSize")}</th><th>{tr("common.colCompleted")}</th><th>{tr("common.colOutputDir")}</th>
          </tr>
        </thead>
        <tbody>
          {#each entries as entry (entry.id || entry.repo + "|" + entry.endedAt)}
            <tr>
              <td>
                <div class="history-repo">{entry.repo}{#if entry.isDataset}<span class="model-tag" style="font-size:11px">{tr("cache.filter.dataset")}</span>{/if}</div>
                {#if entry.revision && entry.revision !== "main"}<div class="history-repo-rev">@ {entry.revision}</div>{/if}
              </td>
              <td>
                <span class="{entry.status === "completed" ? "history-status-completed" : "history-status-failed"}">{entry.status === "completed" ? "✓ " : "✗ "}{tr(entry.status === "completed" ? "jobs.status.completed" : "jobs.status.failed")}</span>
                {#if entry.error}<div style="font-size:11px;color:var(--color-error);margin-top:3px" title={entry.error}>{entry.error}</div>{/if}
              </td>
              <td>{entry.totalFiles ? entry.totalFiles + " " + tr("common.colFiles") : "—"}</td>
              <td>{entry.totalBytes ? formatBytes(entry.totalBytes) : "—"}</td>
              <td class="history-date" title={entry.endedAt || ""}>{entry.endedAt ? formatRelativeDate(entry.endedAt, tr) : "—"}</td>
              <td>
                <div class="history-dir" title={entry.outputDir || ""}>{entry.outputDir || "—"}</div>
                {#if meaningfulHubDir(entry.hubDir, entry.outputDir)}<div class="history-dir-hub" title={entry.hubDir}>Hub: {entry.hubDir}</div>{/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</div>
