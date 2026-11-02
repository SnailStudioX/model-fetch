// Progress events arrive per file. Coalescing keeps one repaint per frame instead
// of one per message. The scheduler is injectable so the behaviour is testable.
export function createCoalescer(target, scheduler = (fn) => requestAnimationFrame(fn)) {
  const pending = new Map();
  let armed = false;

  function flush() {
    armed = false;
    for (const [id, job] of pending) target.set(id, job);
    pending.clear();
  }

  return {
    apply(job) {
      if (!job || !job.id) return;
      pending.set(job.id, job);
      if (!armed) {
        armed = true;
        scheduler(flush);
      }
    },
    // remove forgets a job and any update still queued for it. Dismissal
    // deletes the job server-side; a progress frame that was already waiting
    // for the next frame must not put the card back.
    remove(id) {
      pending.delete(id);
      target.delete(id);
    },
    flush,
    pending
  };
}
