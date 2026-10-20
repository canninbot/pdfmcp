#!/usr/bin/env bash
set -euo pipefail

ROOT=${1:-"$(pwd)/pdfs"}
BINARY=${2:-"$(pwd)/dist/pdfmcp"}
REPETITIONS=${3:-10}
RESULTS_DIR=${4:-"$(pwd)/benchmarks/results/$(date -u +%Y%m%dT%H%M%SZ)"}
GO_TMP=${PDFMCP_GOTMPDIR:-"$(pwd)/.benchmark-go-tmp"}

mkdir -p "$RESULTS_DIR" "$GO_TMP"
ROOT=$(realpath "$ROOT")
BINARY=$(realpath "$BINARY")

if [[ ! -x "$BINARY" ]]; then
	CGO_ENABLED=0 GOTMPDIR="$GO_TMP" go build -trimpath -o "$BINARY" ./cmd/pdfmcp
fi

measure() {
	local name=$1
	shift
	local payload="$RESULTS_DIR/${name}.payload"
	local timing="$RESULTS_DIR/${name}.time"
	local started ended elapsed user system rss bytes
	started=$(date +%s%N)
	/usr/bin/time -f '%U %S %M' -o "$timing" "$@" >"$payload"
	ended=$(date +%s%N)
	elapsed=$(awk -v start="$started" -v end="$ended" 'BEGIN { printf "%.9f", (end-start)/1000000000 }')
	read -r user system rss <"$timing"
	bytes=$(wc -c <"$payload")
	jq -cn --arg name "$name" --argjson elapsed "$elapsed" \
		--argjson user "$user" --argjson system "$system" \
		--argjson rss_kib "$rss" --argjson bytes "$bytes" \
		'{test:$name,elapsed_s:$elapsed,user_s:$user,system_s:$system,max_rss_kib:$rss_kib,payload_bytes:$bytes}' \
		>>"$RESULTS_DIR/timings.jsonl"
}

# Warm long-lived filesystem and SQLite caches before timed repetitions.
"$BINARY" search sin3 --root "$ROOT" --mode literal --scope text --output pmids --limit 200 >/dev/null
rg -i -l -F sin3 --glob markdown.md "$ROOT" >/dev/null

for repetition in $(seq 1 "$REPETITIONS"); do
	measure "cli_literal_pmids_text_${repetition}" "$BINARY" search sin3 --root "$ROOT" --mode literal --scope text --output pmids --limit 200
	measure "cli_literal_snippets_text_${repetition}" "$BINARY" search sin3 --root "$ROOT" --mode literal --scope text --output snippets --limit 200
	measure "cli_literal_full_all_${repetition}" "$BINARY" search sin3 --root "$ROOT" --mode literal --scope all --output full --limit 200
	measure "cli_regex_pmids_all_${repetition}" "$BINARY" search sin3 --root "$ROOT" --mode regex --scope all --output pmids --limit 200
	measure "cli_ranked_${repetition}" "$BINARY" search sin3 --root "$ROOT" --mode ranked --scope all --output full --limit 200
	measure "cli_common_word_${repetition}" "$BINARY" search the --root "$ROOT" --mode literal --scope text --output pmids --limit 200
	measure "cli_no_match_${repetition}" "$BINARY" search pdfmcp-definitely-absent --root "$ROOT" --mode literal --scope text --output pmids --limit 200
	measure "rg_pmids_${repetition}" rg -i -l -F sin3 --glob markdown.md "$ROOT"
	measure "rg_lines_${repetition}" rg -i -n -F sin3 --glob markdown.md "$ROOT"
done

PDFMCP_BENCH_ROOT="$ROOT" GOTMPDIR="$GO_TMP" \
	go test ./internal/search -run '^$' -bench '^BenchmarkReal' \
	-benchtime="${REPETITIONS}x" -count=1 -benchmem \
	-cpuprofile="$RESULTS_DIR/after_cpu.pprof" -memprofile="$RESULTS_DIR/after_mem.pprof" \
	>"$RESULTS_DIR/direct_go.txt"

PDFMCP_BENCH_ROOT="$ROOT" GOTMPDIR="$GO_TMP" \
	go test ./internal/mcpserver -run '^$' -bench '^BenchmarkRealMCP' \
	-benchtime="${REPETITIONS}x" -count=1 -benchmem \
	>"$RESULTS_DIR/mcp_go.txt"

jq -s '
	group_by(.test | sub("_[0-9]+$"; "")) |
	map({test:(.[0].test | sub("_[0-9]+$"; "")), repetitions:length,
		times:(map(.elapsed_s) | sort),
		min_s:(map(.elapsed_s) | min),
		median_s:(map(.elapsed_s) | sort | .[length/2]),
		p95_s:(map(.elapsed_s) | sort | .[((length * 95 + 99) / 100 | floor) - 1]),
		max_s:(map(.elapsed_s) | max),
		mean_cpu_s:(map(.user_s + .system_s) | add / length),
		peak_rss_kib:(map(.max_rss_kib) | max),
		payload_bytes:(map(.payload_bytes) | max)})' \
	"$RESULTS_DIR/timings.jsonl" >"$RESULTS_DIR/summary.json"

rg -i -l -F sin3 --glob markdown.md "$ROOT" | sed -E 's#.*/([0-9]+)/markdown.md#\1#' | sort -u >"$RESULTS_DIR/rg_pmids.txt"
"$BINARY" search sin3 --root "$ROOT" --mode literal --scope text --output pmids --limit 200 \
	| jq -r '.pmids[]' | sort -u >"$RESULTS_DIR/pdfmcp_pmids.txt"
diff -u "$RESULTS_DIR/rg_pmids.txt" "$RESULTS_DIR/pdfmcp_pmids.txt" >"$RESULTS_DIR/pmid_diff.txt" || true

printf 'benchmark results: %s\n' "$RESULTS_DIR"
