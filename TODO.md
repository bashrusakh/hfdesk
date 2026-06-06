# TODO

## README rendering

- Implement the full Hugging Face README renderer after the minimal version:
  server-side README fetching for all analyzable repo types, sanitized Markdown,
  relative image/link rewriting, optional authenticated asset proxy for gated
  repos, lazy-loaded images, collapsible long sections, and polished table/code
  styling.

## Done

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
