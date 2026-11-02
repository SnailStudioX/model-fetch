// Module-scoped tally of in-flight HTTP requests. It lives here rather than in
// store.svelte.js because the store imports api.js, and api.js needs to report
// into this state — importing the store back would close that cycle.
//
// The visible flag trails the count by a short delay: requests that answer
// quickly never flash the indicator, and only the ones where the data has
// genuinely not come back yet show it.
//
// Hard constraint: globalLoading must only be WRITTEN here, never read.
// apiFetch is routinely called straight from $effect bodies, and inside one,
// reading $state makes that effect depend on it — the tally's own increments
// would then invalidate the effect that made the request, and it would re-run,
// re-request, and loop until Svelte's infinite-loop guard throws. The counter
// and the reveal bookkeeping therefore stay in plain variables, and visibleNow
// mirrors the reactive flag so the logic never reads it back.
export const globalLoading = $state({ visible: false });

const REVEAL_MS = 250;
let revealTimer = null;
let pending = 0;
let visibleNow = false;

export function requestStarted() {
  pending += 1;
  if (revealTimer === null && !visibleNow) {
    revealTimer = setTimeout(() => {
      revealTimer = null;
      if (pending > 0) {
        visibleNow = true;
        globalLoading.visible = true;
      }
    }, REVEAL_MS);
  }
}

export function requestFinished() {
  if (pending > 0) pending -= 1;
  if (pending === 0) {
    if (revealTimer !== null) {
      clearTimeout(revealTimer);
      revealTimer = null;
    }
    if (visibleNow) {
      visibleNow = false;
      globalLoading.visible = false;
    }
  }
}
