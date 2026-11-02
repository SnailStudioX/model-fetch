<script>
  import { ui, runJobAction, openRepoDetail, toast } from "../store.svelte.js";
  import { t, tf } from "../i18n.js";
  import { formatBytes, formatDuration } from "../format.js";

  let { job } = $props();
  const tr = (key) => t(key, ui.lang);
  const statusLabel = (value) => {
    const key = "jobs.status." + value;
    const text = t(key, ui.lang);
    return text === key ? value : text;
  };

  const progress = $derived(job.progress || {});
  const totalBytes = $derived(progress.totalBytes || 0);
  const downloadedBytes = $derived(progress.downloadedBytes || 0);
  // A job that never got a plan (a failed tree API call, a cancelled queue entry)
  // has no total. "0.0% - 255.7 MB / 0 B" reads as a broken meter rather than as
  // a job that failed before planning, so the unknown total is named instead.
  const hasTotal = $derived(totalBytes > 0);
  const pct = $derived(hasTotal ? Math.min(100, (downloadedBytes / totalBytes) * 100) : 0);
  const status = $derived(job.status || "queued");
  const displayStatus = $derived(status === "running" && job.phase === "finalizing" ? "finalizing" : status);
  // Only a transferring job has a live speed. Finalizing is local I/O and the
  // server zeroes the reading there; a paused job keeps its last reading on
  // purpose, so it is shown as a past value instead of pretending bytes still
  // move, which is what made the speed look like it had vanished.
  const transferring = $derived(status === "running" && job.phase !== "finalizing");
  const speed = $derived((transferring && progress.bytesPerSecond) || 0);
  const lastSpeed = $derived(!transferring && progress.bytesPerSecond > 0 ? progress.bytesPerSecond : 0);
  const pending = $derived(ui.jobActions.has(job.id));
  const labels = $derived.by(() => {
    const filters = Array.isArray(job.filters) ? job.filters : [];
    return {
      quant: filters.find((filter) => !/mmproj/i.test(filter)) || (filters.length ? filters.join(", ") : ""),
      mmproj: filters.find((filter) => /mmproj/i.test(filter)) || ""
    };
  });
  const files = $derived(job.files || []);
  const activeFiles = $derived(files.filter((file) => file.status === "active" || file.status === "finalizing"));
  const activeFile = $derived(files.find((file) => file.status === "active") || files.find((file) => file.status === "finalizing") || files.find((file) => file.status === "pending") || files.slice(-1)[0] || null);
  // Several files download at once. Naming only one of them made the others look
  // lost, so the count of the rest is shown next to it.
  const extraFiles = $derived(Math.max(0, activeFiles.length - 1));
  // A file is not finished when its bytes arrived. With several connections per file
  // the parts still have to be stitched into one payload and the result hashed, which
  // is local disk work with no network speed attached. Without naming that stage the
  // card reads "100% and done" while the disk is still rewriting tens of GB.
  const stage = $derived(activeFile?.status === "finalizing" ? activeFile.stage || "" : "");
  const stageLabel = $derived.by(() => {
    if (!stage) return "";
    const key = "jobs.stage." + stage;
    const text = t(key, ui.lang);
    const name = text === key ? stage : text;
    const total = activeFile?.stageTotal || 0;
    const done = activeFile?.stageBytes || 0;
    const share = total > 0 ? ` ${Math.min(100, Math.round((done / total) * 100))}%` : "";
    // The rate is the local disk throughput of that work, kept inside the chip
    // so it can never be mistaken for the download speed, which is zero here.
    const rate = stageSpeed > 0 ? ` · ${formatBytes(stageSpeed)}/s` : "";
    return `${name}${share}${rate}`;
  });
  // Stage bytes arrive about once a second, so the difference between two
  // readings is the local rate. A new file, a new stage or a counter that
  // went backwards starts the window again instead of reporting a jump.
  let stageSpeed = $state(0);
  let stageSample = { key: "", at: 0, bytes: 0 };
  $effect(() => {
    const key = `${job.id}|${activeFile?.path || ""}|${stage}`;
    const bytes = stage ? activeFile?.stageBytes || 0 : 0;
    const now = Date.now();
    if (!bytes) {
      stageSample = { key: "", at: 0, bytes: 0 };
      stageSpeed = 0;
      return;
    }
    if (key !== stageSample.key || bytes < stageSample.bytes) {
      stageSample = { key, at: now, bytes };
      stageSpeed = 0;
      return;
    }
    const elapsed = (now - stageSample.at) / 1000;
    if (elapsed >= 0.5) {
      stageSpeed = (bytes - stageSample.bytes) / elapsed;
      stageSample = { key, at: now, bytes };
    }
  });
  // A finished transfer with local work still running leaves the share at 100%,
  // which is exactly the reading that looked like a stuck job, so the fill
  // breathes while the stage is running.
  const finalizingBar = $derived(Boolean(stage) || job.phase === "finalizing");
  const metaVisible = $derived(Boolean(job.revision && job.revision !== "main") || Boolean(labels.quant) || Boolean(labels.mmproj));
  // A 70 GB job at 0.3% is a sub-pixel fill, which reads as "nothing is
  // happening" even though bytes did move, so a nonzero share keeps a visible sliver.
  const indeterminate = $derived(transferring && !hasTotal);
  const fillWidth = $derived(indeterminate ? "34%" : pct > 0 ? `max(3px, ${pct}%)` : "0%");
  const progressTitle = $derived(hasTotal
    ? `${pct.toFixed(1)}% - ${formatBytes(downloadedBytes)} / ${formatBytes(totalBytes)}`
    : formatBytes(downloadedBytes));

  async function openRepo() {
    try {
      await openRepoDetail(job.repo, job.revision || "");
    } catch (err) {
      // The page did switch, so a failed analyze has to be named here or the click
      // still reads as a dead button.
      toast(tr("common.failed") + ": " + (err?.message || tr("common.error")), "error");
    }
  }

  async function act(action) {
    try {
      await runJobAction(job.id, action);
    } catch (err) {
      toast(err.message || tr("common.error"), "error");
    }
  }
</script>

<div class="job-card" data-job-id={job.id} data-status-category={status}>
  <div class="job-header">
    <div>
      <div class="job-repo job-repo-clickable" title={tr("common.viewModelDetails")} role="button" tabindex="0" onclick={openRepo} onkeydown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); openRepo(); } }}>{job.repo}</div>
      <div class="job-meta-line" style={metaVisible ? "" : "display:none"}>
        <span style={job.revision && job.revision !== "main" ? "" : "display:none"}>{job.revision && job.revision !== "main" ? "@ " + job.revision : ""}</span>
        <span class="job-chip" style={labels.quant ? "" : "display:none"}>{labels.quant}</span>
        <span class="job-chip job-chip-vision" style={labels.mmproj ? "" : "display:none"}>mmproj</span>
      </div>
    </div>
    <div class="job-header-right">
      <span class="job-status {displayStatus}">{statusLabel(displayStatus)}</span>
      <span class="job-actions">
        {#if status === "running"}
          <button class="btn btn-sm btn-warning" type="button" disabled={pending} onclick={() => act("pause")}>{tr("jobs.pause")}</button>
          <button class="btn btn-sm btn-danger" type="button" disabled={pending} onclick={() => act("cancel")}>{tr("jobs.cancel")}</button>
        {:else if status === "paused"}
          <button class="btn btn-sm btn-primary" type="button" disabled={pending} onclick={() => act("resume")}>{tr("jobs.resume")}</button>
          <button class="btn btn-sm btn-danger" type="button" disabled={pending} onclick={() => act("cancel")}>{tr("jobs.cancel")}</button>
        {:else if status === "queued"}
          <button class="btn btn-sm btn-danger" type="button" disabled={pending} onclick={() => act("cancel")}>{tr("jobs.cancel")}</button>
        {:else if status === "failed" || status === "cancelled"}
          <button class="btn btn-sm btn-primary" type="button" disabled={pending} onclick={() => act("retry")}>{tr("jobs.retry")}</button>
          <button class="btn btn-sm btn-secondary" type="button" disabled={pending} onclick={() => act("dismiss")}>{tr("jobs.dismiss")}</button>
        {:else}
          <button class="btn btn-sm btn-secondary" type="button" disabled={pending} onclick={() => act("dismiss")}>{tr("jobs.dismiss")}</button>
        {/if}
      </span>
    </div>
  </div>
  <div class="job-progress">
    <div class="progress-bar{indeterminate ? " indeterminate" : ""}{finalizingBar ? " is-finalizing" : ""}" title={progressTitle}>
      <div class="progress-fill" style={`width: ${fillWidth}`}></div>
    </div>
  </div>
  <div class="job-stats">
    <span data-role="pct">{hasTotal ? pct.toFixed(1) + "%" : tr("jobs.pctUnknown")}</span>
    <span data-role="speed" style={speed > 0 ? "" : "display: none"}>{speed > 0 ? formatBytes(speed) + "/s" : ""}</span>
    <span data-role="last-speed" style={lastSpeed > 0 ? "" : "display: none"}>{lastSpeed > 0 ? tf("jobs.lastSpeed", ui.lang, { speed: formatBytes(lastSpeed) }) : ""}</span>
    <span data-role="eta" style={transferring && progress.etaSeconds > 0 && hasTotal && totalBytes > downloadedBytes ? "" : "display: none"}>{transferring && progress.etaSeconds > 0 && hasTotal && totalBytes > downloadedBytes ? formatDuration(progress.etaSeconds, tr) : ""}</span>
    <span data-role="size">{formatBytes(downloadedBytes)}{hasTotal ? " / " + formatBytes(totalBytes) : ""}</span>
    <span data-role="files">{tr("jobs.completedFiles")}: {(progress.completedFiles || 0)} / {(progress.totalFiles || 0)} {tr("common.colFiles")}</span>
  </div>
  <div class="job-file-line" style={activeFile?.path ? "" : "display:none"} title={activeFile?.path || ""}>
    <span class="job-file-name">{activeFile?.path ? `${statusLabel(activeFile.status || "file")}: ${activeFile.path}` : ""}</span>
    <span class="job-stage" data-role="stage" title={tr("jobs.stageHint")}
      style={stageLabel ? "" : "display:none"}>{stageLabel}</span>
    <span class="job-file-more" style={extraFiles > 0 ? "" : "display:none"}>{extraFiles > 0 ? tf("jobs.parallelFiles", ui.lang, { n: extraFiles }) : ""}</span>
  </div>
  <div class="job-error" style={job.error ? "" : "display: none"}>{job.error || ""}</div>
</div>