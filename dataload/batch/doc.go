// Package batch implements manifest-based batch imports for dataload.
//
// Contract with the producer: it writes every file atomically (.tmp + rename)
// and manifest.json LAST, as the "batch complete" signal:
//
//	{"files": {"corpus.json": "<sha256>", ...}, "generated_at": "...", "producer": "...", "schema": "..."}
//
// The consumer declares a Contract (fixed file names, import order, optional
// files) and calls Process on a polling loop (Watcher). One Process call is one
// revision of the directory and it:
//
//   - does nothing when manifest.json is absent (no complete batch yet);
//   - skips a manifest whose sha256 was already processed, and also a new
//     manifest whose files (their sha256) equal the last processed batch: a
//     producer that regenerates generated_at on every run does not trigger a
//     re-import (ContentPrint ignores generated_at);
//   - never opens a name that is not declared in the Contract (a manifest entry
//     such as "../x" is only reported as unknown), and caps the size of every
//     file before reading it;
//   - verifies the sha256 of ALL files before importing any, so a half-written
//     batch is never imported halfway;
//   - imports in the Contract's order and, if an import fails, records nothing:
//     the next revision retries the whole batch, so imports must be idempotent;
//   - records the batch in the Ledger only after every file imported.
//
// Persistence is an interface (Ledger); NewMemoryLedger is for tests and
// single-process tools, and batch/gormledger is a SQL implementation. The
// package does not use fsnotify on purpose: the poll already covers it and it
// avoids a dependency.
//
// Messages are dataload.Message values with stable codes (dataload.batch.*) and
// errors are dataload.Error, so hosts localize them like the rest of dataload.
package batch
