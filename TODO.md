# TODO

(no open items)

## Done

- Full README renderer: server-side sanitized Markdown (goldmark + bluemonday,
  GFM tables/code), relative image/link rewriting, authenticated image proxy
  for gated repos, lazy images; full description shown by default.
- HF Hub button in the model detail panel (right column).
- Smooth download speed; show real current throughput (excludes resumed bytes).
- Lowering max-active re-queues excess jobs (auto-resume), not pause.
- tools/reasoning badges in local cache (detected from name/tags).
- Description (README) shown below the quantizations list.
- Local cache defaults to List view.
- Finalizing status on the job card during post-download processing.
- Quant download buttons disable while that download is active/queued.
- Web UI version read from the build automatically.

- Sort Active Jobs / downloads list by date added, newest first.
- Auto-move completed downloads out of Active Jobs into History (failed/
  cancelled/paused stay visible).
- Fix: Pause button missing after a download starts (queued->running button
  refresh).
- Add Retry button for failed/cancelled downloads.
- Compact download row height; hide redundant `main` revision label.
- Enforce `max-active` as a concurrent-jobs limit: scheduler queues jobs above
  the limit and starts the next when a slot frees; lowering the limit pauses
  the most-recently-started excess jobs (oldest keep running).
- GGUF-only downloads: when a filter targets a `.gguf` file, fetch only the
  selected quant (+ mmproj) and skip config/tokenizer/README/`fp16/` clutter.
