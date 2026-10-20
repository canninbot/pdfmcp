# Search optimization report

Measured on 2026-10-19 on Linux/amd64, Go 1.26.0, an Intel Xeon Gold
6242R, and a warm filesystem cache. The corpus contains 16,131 papers,
2,305,456 searchable blocks, 145,998 known pages, 155,643 figures, and
39,329 tables. Every reported distribution has one warm-up followed by 10
repetitions. p95 uses nearest-rank, so it is the maximum for 10 samples.

## Finding the original bottleneck

The original literal `scope=all` path took a median 17.683 s. It listed and
fully hydrated document metadata, then issued `AllBlocks` once for each of
16,131 papers. Because the 33 matching papers are distributed through the
corpus, it decoded nearly all 2.3 million blocks, including unused captions,
bounding boxes, image paths, and source fields. It also lowercased individual
lines. The search did not parse MinerU JSON or open Markdown in this mode.

The baseline allocation profile attributes 82.0% of 4.91 GiB to `ScanBlocks`
and its descendants, including 1.82 GiB in the SQLite driver's `columnText`.
The measured benchmark was 5.07 GB/op and 59.1 million allocations/op. MCP
calls took approximately the same time as direct/CLI calls, so neither JSON
serialization nor transport explained the former 117x comparison.

The new path first finds candidate documents and then verifies exact semantics
against original text. Full output uses one streaming, column-minimal SQL
query. PMID output never creates match records or hydrates metadata. Markdown
scans use bounded concurrency, reusable buffers, cached indexed paths, an
ASCII-compatible fast matcher, and a Unicode-exact fallback. Regexes compile
once per request.

## Final results

Wall-clock values below are CLI medians. The before API did not have PMID-only
output; its closest text equivalent returned full match records and is not
used to claim a PMID speedup. A timeout means PDFMCP's 30-second request
deadline was reached.

| Test | Before | After | ripgrep | Speedup |
|---|---:|---:|---:|---:|
| Literal PMID-only, Markdown | unavailable | 0.224 s | 0.145 s | N/A |
| Literal snippets, Markdown | 4.696 s | 0.227 s | 0.155 s | 20.7x |
| Literal full, scope=all | 17.683 s | 0.329 s | N/A | 53.7x |
| Regex PMID-only, scope=all | >30 s timeout | 0.334 s | 0.145 s* | >89.9x |
| Ranked FTS5 | 0.0158 s | 0.0162 s | N/A | within noise |
| Common word, first 200 PMIDs | 0.238 s | 0.087 s | N/A | 2.73x |
| No-match literal, Markdown | 4.475 s | 0.209 s | N/A | 21.5x |

`*` The regex is the plain string `sin3`, so the measured fixed-string
filename result is an equivalent reference result.

The equivalent PMID result contained the same 33 PMIDs. In-process PDFMCP
needed 0.163 s and long-lived in-memory MCP needed 0.170 s, versus ripgrep's
0.145 s CLI median: approximately 1.12x and 1.17x respectively. Cold CLI
startup/path validation raises PDFMCP to 1.54x ripgrep. All are below one
second and the long-lived server is within the approximately-2x target.

| Workload | Direct Go | CLI median / p95 / min / max | MCP in-memory |
|---|---:|---:|---:|
| Literal PMID text | 0.163 s | 0.224 / 0.244 / 0.203 / 0.244 s | 0.170 s |
| Literal snippets text | 0.176 s | 0.227 / 0.272 / 0.221 / 0.272 s | 0.202 s |
| Literal full all | 0.293 s | 0.329 / 0.343 / 0.302 / 0.343 s | 0.311 s |
| Regex PMID all | 0.302 s | 0.334 / 0.359 / 0.325 / 0.359 s | 0.317 s |
| Ranked full | 0.00116 s | 0.0162 / 0.0183 / 0.0112 / 0.0183 s | 0.00437 s |

Direct full literal search now allocates 44.3 MB/op in 868,894 allocations,
down from 5.07 GB/op and 59.1 million allocations. Full CLI peak RSS fell
from 94,300 KiB to 27,364 KiB. PMID-only CLI uses at most 69,696 KiB in the
measured run. Baseline full search consumed 22.69 aggregate CPU seconds per
request; final full search consumed 0.44 seconds. The CLI full payload is
95,816 bytes; PMID-only is 673 bytes. JSON marshaling of an already-built full
response takes 0.155 ms (four allocations), so serialization is below 0.1% of
the former 17.7-second latency. The MCP rich result is larger because the SDK
returns both text and structured content, but transport adds only milliseconds
to search latency.

## Index and planner

Schema version 7 adds a contentless, document-level FTS5 trigram table with
separate body, figure, and table columns. It stores postings but not another
copy of source text. The database grew from 2,697,031,680 to 2,958,737,408
bytes: 261,705,728 bytes or 9.70%. Rebuilding this accelerator from existing
SQLite blocks took approximately 116 seconds; MinerU JSON was not parsed.
Incremental indexing writes it in the same transaction as each document.

The planner selects:

- ranked FTS5/BM25 for ranked mode;
- bounded native-Go Markdown scanning for text PMID/snippet results;
- document trigram candidates plus exact verification for eligible literal
  and regex searches over indexed blocks;
- a full streaming verification scan for one/two-character literals and
  regexes without a guaranteed literal prerequisite.

The trigram text and query use a canonical Unicode simple-fold representative.
That makes the prefilter a superset for both literal lowercasing and RE2
case-folding. It can add candidates but cannot discard a valid result. Exact
verification always decides the result. `debug: true` (or CLI `--debug`)
returns the chosen strategy and reason without writing protocol diagnostics to
stdout.

SQLite's plan for the candidate step scans the FTS5 virtual index, joins the
small document mapping by integer primary key, and sorts PMIDs for deterministic
output. Candidate block queries use the existing `blocks_pmid_page` index.
No ordinary query parses structured JSON, and `scope=all` searches canonical
indexed blocks rather than searching duplicate Markdown plus JSON content.

## Correctness

The opt-in `TestRealCorpusExactPMIDs` independently scanned all 2,305,456
blocks once, then compared complete paginated PMID sets for all requested
identifiers plus figure/table scopes and safe-prefilter regexes. It passed in
149.5 seconds. Synthetic tests additionally cover short strings, no-match,
case sensitivity, Unicode simple-fold edge cases, punctuation, output levels,
pagination, schema migration, and all scopes.

Markdown comparisons against `rg -a -i -l -F` had zero set differences:

| Query | Matching PMIDs |
|---|---:|
| sin3 | 33 |
| SIN3A | 21 |
| NCT04350463 | 8 |
| TEAD | 3,144 |
| KRAS | 1,147 |
| IL-6 | 480 |
| p53 | 3,098 |
| β-catenin | 297 |
| thrombocytopenia | 2,140 |

Without ripgrep's `-a/--text`, 12 TEAD papers and smaller numbers for several
other queries are omitted because their MinerU Markdown contains NUL bytes and
ripgrep classifies it as binary. PDFMCP intentionally searches that content.
For `sin3`, both the original reference command and PDFMCP return the same 33
PMIDs. The line benchmark found the same 79 `(PMID, line)` locations; PDFMCP
truncates snippets beyond 2,000 Unicode characters, unlike raw `rg -n` lines.

## Reproduction

Run:

```bash
make current
./benchmarks/run_search_benchmarks.sh "$PWD/pdfs" "$PWD/dist/pdfmcp" 10
PDFMCP_TEST_ROOT="$PWD/pdfs" go test ./internal/search -run TestRealCorpusExactPMIDs -count=1 -v
```

Raw final data is in `benchmarks/results/final2-20261019/`; baseline timing,
profiles, and the preserved baseline executable are in
`benchmarks/results/baseline-20261019/`. The untouched baseline database and
source snapshot used here are `/tmp/pdfmcp-baseline-cuOwXe/`.
