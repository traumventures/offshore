// Package anyfs is the one storage core behind offshore's file systems:
// build slot caches on workers, the source projection (sourcefs) and the
// laptop's output overlay (outputfs) are configurations of it.
//
// Data model. A Tree is a manifest: every path with kind, mode, size,
// mtime, the BLAKE3 digest of its content and the pages holding it. A
// page (the unit of transfer and caching, about PageSize) is content
// addressed by its Key; small files are packed into shared pages, large
// files split into several. A Layer is a delta of a Tree (changed nodes
// and whiteouts) with a parent; a chain of layers folds into the Tree of
// a build slot.
//
// Tiers. A Tier stores pages: local disk, an attachable volume, the
// object store, a peer over QUIC. Link(upper, lower) makes a pair of
// tiers a Tier again, so stacks fold recursively (local -> volume ->
// store, local -> peer -> store). Each link has a Policy: inclusion
// (inclusive or exclusive/victim), write-through or write-back with
// dirty thresholds, readahead, reclaim and promote/demote.
//
// Virtual memory naming. The local tier is RAM, the lower tiers are swap
// and the store is the backing store. A page is resident when it is in
// the local tier, clean when a lower tier holds it too (reclaim is free)
// and dirty until it is written back. A lookup that misses the upper tier
// is a page fault; fetching it is a page-in, writing it to the lower tier
// a page-out or writeback; dropping resident pages is reclaim; pinned
// pages are never reclaimed. The working set is the bytes touched within
// a window; when it exceeds the local capacity the stack thrashes
// (refaults of recently reclaimed pages), which the scheduler answers
// with a bigger disk or machine. Events: page_fault, page_in, page_out,
// writeback, reclaim, thrash; metrics: resident_bytes, dirty_bytes,
// working_set_bytes, fault_rate, reclaim_bytes.
//
// File systems. Files materializes the content of tree nodes as local
// read-only files (the page cache of whole files). LowerFS serves a
// Source (a Tree with its Files, a directory, a remote listing) as a
// read-only FUSE file system with kernel passthrough, fetch on first
// read (never on open) and prefetch of small files. On privileged hosts
// (workers) a kernel overlayfs (redirect_dir, metacopy) stacks a local
// upper directory over it; unprivileged hosts (laptops) use the
// userspace overlay (UserOverlay) with whiteouts and a merge policy
// (top-wins or newest-mtime).
package anyfs
