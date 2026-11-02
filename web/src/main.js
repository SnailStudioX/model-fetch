import { mount, unmount } from "svelte";
import App from "./App.svelte";
import { ui, connect, loadJobs, loadSettings, loadHealth, toast } from "./store.svelte.js";
import { isStaleBuild } from "./api.js";

const target = document.querySelector("#app");
document.documentElement.lang = ui.lang;
mount(App, { target });

// A failed first load has to be visible: the Jobs page would otherwise render an
// empty list and the Cache page an empty library. The toast is what the user
// sees; loadSettings additionally records the failure for the pages that can
// show it inline.
loadSettings().catch((err) => toast(err.message, "error"));
loadJobs().catch((err) => toast(err.message, "error"));
loadHealth().catch(() => {});

const ws = connect(
  (job) => {
    // Completion notifications belong to the shell, not the renderer.
    if (job.status === "completed" && window.electronAPI?.notify) {
      window.electronAPI.notify(job.repo + " finished");
    }
  },
  {
    // The socket dropping is the signal that the server was restarted. If the page is
    // still running a bundle that server no longer serves, nothing on screen can
    // be trusted to match the binary, so reload instead of arguing about it.
    onReconnect: () => {
      isStaleBuild()
        .then((stale) => {
          if (stale) location.reload();
        })
        .catch(() => {});
    }
  }
);

// Cancellation must be explicit: closing the socket stops the reconnect timer,
// otherwise a destroyed page keeps retrying a dead connection.
window.addEventListener("beforeunload", () => {
  ws.close();
  unmount(target);
});