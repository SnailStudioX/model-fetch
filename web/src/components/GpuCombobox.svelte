<script module>
  let comboboxSeq = 0;
</script>

<script>
  import { t } from "../i18n.js";
  import { displayGpuName } from "../format.js";

  let { value, entries = [], label = "", lang = "zh", onpick } = $props();

  // The list is the canirun.ai device catalog already loaded for the profile, so
  // a pick fills VRAM, bandwidth and core count from the same row the search uses.
  // Names are shown in short form ("RTX 3090"), which is also what gets committed.
  let text = $state("");
  let committed = $state("");
  // The filter is what the user typed since focusing. The committed name is only
  // what the box shows: filtering by it would leave a picked card as the only
  // row in the list, so the pick could never be changed.
  let query = $state("");
  let open = $state(false);
  let active = $state(0);
  let focused = $state(false);
  const listId = `gpu-combobox-list-${comboboxSeq++}`;

  // Reading text here would make the effect depend on the user's typing and reset
  // it on every keystroke, so both values come from the prop.
  $effect(() => {
    if (focused) return;
    const display = displayGpuName(value);
    text = display;
    committed = display;
  });

  const tr = (key) => t(key, lang);
  const rows = $derived(entries.map((entry) => ({ ...entry, display: displayGpuName(entry.name) })));
  const filtered = $derived(rows.filter((row) => !query || row.display.toLowerCase().includes(query.toLowerCase())).slice(0, 12));

  // Blur must not re-commit the filter text: a click on an option blurs the input
  // first, so without this guard the typed fragment wins over the picked card.
  function commit(name) {
    const clean = displayGpuName(String(name || "").trim());
    if (!clean || clean === committed) return;
    committed = clean;
    text = clean;
    onpick?.(clean);
    open = false;
  }

  function openList() {
    open = true;
    const picked = filtered.findIndex((row) => row.display === displayGpuName(value));
    active = picked >= 0 ? picked : 0;
  }

  function onKeydown(event) {
    if (event.key === "ArrowDown") {
      open = true;
      active = Math.min(active + 1, filtered.length - 1);
      event.preventDefault();
    } else if (event.key === "ArrowUp") {
      open = true;
      active = Math.max(active - 1, 0);
      event.preventDefault();
    } else if (event.key === "Enter") {
      commit(filtered[active]?.display || text);
      event.preventDefault();
    } else if (event.key === "Escape") {
      open = false;
    }
  }
</script>

<div class="gpu-combobox">
  <input
    class="gpu-combobox-input"
    value={text}
    placeholder={tr("settings.searchGpu")}
    aria-label={label}
    role="combobox"
    aria-expanded={open}
    aria-controls={listId}
    autocomplete="off"
    spellcheck="false"
    oninput={(event) => {
      text = event.currentTarget.value;
      query = text;
      open = true;
      active = 0;
    }}
    onfocus={() => {
      focused = true;
      query = "";
      openList();
    }}
    onblur={() => {
      focused = false;
      open = false;
      query = "";
      if (text !== committed) commit(text);
    }}
    onkeydown={onKeydown}
  />
  {#if open && filtered.length}
    <div class="gpu-combobox-list" id={listId} role="listbox">
      {#each filtered as row, index (row.name)}
        <button
          type="button"
          role="option"
          aria-selected={row.display === displayGpuName(value)}
          class={index === active ? "gpu-combobox-active" : ""}
          onmousedown={(event) => event.preventDefault()}
          onclick={() => commit(row.display)}
        >
          <span class="gpu-combobox-name">{row.display}</span>
          <span class="gpu-combobox-spec">
            {row.vram_gb ? row.vram_gb + " GB" : "?"} · {row.bandwidth_gbps ? row.bandwidth_gbps + " GB/s" : "?"} · {row.cores ? row.cores : "?"}
          </span>
        </button>
      {/each}
    </div>
  {/if}
</div>