#!/usr/bin/env bash
# Genomic data and shared data (spec §9.6, §9.8, §11): stack A loads the
# synthetic genomic fixture (internal/testfixtures/genomic) and answers its
# expected.json queries, then publishes its data as a shared set and is
# destroyed. Stack B mounts the set read-only, hydrates its dictionary and
# must answer the same queries. Needs docker, git, go and jq; settings are
# in scripts/e2e-lib.sh, plus E2E_SHARED_SET (the set's name, default
# ci-genomic-<time>-<pid>).

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

base="${E2E_NAME:-e2e-genomic}"
a="$base-a" b="$base-b"
dir_a="$E2E_WORK/$a" dir_b="$E2E_WORK/$b"
set_name="${E2E_SHARED_SET:-ci-genomic-$(date +%Y%m%d%H%M%S)-$$}"
fixture="$E2E_WORK/fixture"
expected="$repo_root/testdata/genomic/expected.json"

# hpds_dataframe NAME BODY runs a DATAFRAME query through the asynchronous
# API (submit, poll the status, fetch the CSV): this release's /query/sync
# answers DATAFRAME with HTTP 400.
hpds_dataframe() {
	# shellcheck disable=SC2016 # expanded by the container's shell
	docker run --rm --network "$1_query" "$helper" sh -euc '
		post() { wget -q -O - --header "Content-Type: application/json" --post-data "$2" "http://hpds:8080/PIC-SURE/v3/$1"; }
		id="$(post query "$1" | sed -n "s/.*\"resourceResultId\":\"\([^\"]*\)\".*/\1/p")"
		[ -n "$id" ] || { echo "no query id" >&2; exit 1; }
		tries=60
		until status="$(post "query/$id/status" "{}")" && [ "${status#*\"status\":\"AVAILABLE\"}" != "$status" ]; do
			case "$status" in *\"status\":\"ERROR\"*) echo "query $id failed: $status" >&2; exit 1 ;; esac
			tries=$((tries - 1))
			[ "$tries" -gt 0 ] || { echo "query $id never became available" >&2; exit 1; }
			sleep 1
		done
		post "query/$id/result" "{}"' sh "$2" < /dev/null
}

# check_queries NAME runs every expected.json query against the stack's HPDS,
# as a patient list (DATAFRAME) and as a COUNT, and fails on any difference.
check_queries() {
	local name="$1" n i q query got want count bad=0
	n="$(jq '.queries | length' "$expected")"
	for ((i = 0; i < n; i++)); do
		q="$(jq -c ".queries[$i]" "$expected")"
		query="$(jq -c '
			if (.phenotypeFilters // [] | length) > 1 then error("only one phenotype filter is supported") else . end |
			{select: ["\\Genomic Fixture\\Sex\\"], genomicFilters: (.genomicFilters // []), expectedResultType: "DATAFRAME"} +
			(.phenotypeFilters[0] // null | if . then {phenotypicClause: {phenotypicFilterType: "FILTER",
				conceptPath: .conceptPath, values: .values, not: false}} else {} end)' <<< "$q")"
		want="$(jq -c '.patients' <<< "$q")"
		got="$(hpds_dataframe "$name" "{\"query\":$query}" |
			awk -F, 'NR > 1 && $1 != "" { gsub(/"/, "", $1); print int($1) }' | sort -n | jq -cs .)" || got=error
		count="$(hpds "$name" query/sync "$(jq -c '{query: (. + {select: [], expectedResultType: "COUNT"})}' <<< "$query")")" ||
			count=error
		if [ "$got" = "$want" ] && [ "$count" = "$(jq length <<< "$want")" ]; then
			echo "ok   $(jq -r .name <<< "$q") $got" >&2
		else
			echo "BAD  $(jq -r .name <<< "$q"): got $got (count $count), want $want" >&2
			bad=$((bad + 1))
		fi
	done
	[ "$bad" -eq 0 ] || fail "$name: $bad of $n genomic queries differ from expected.json"
}

[ -z "$(docker volume ls -q --filter "name=^${set_name}_")" ] ||
	fail "a data set named $set_name exists; set E2E_SHARED_SET to a new name"

say "genomic fixture"
(cd "$repo_root" && go run ./internal/testfixtures/genomic/cmd/genomic-fixture -abs "$fixture")

init_stack "$a" "$dir_a"

say "$a: load the phenotypes, then the genomic data"
pic --stack "$dir_a" data load-phenotype --file "$fixture/phenotype.csv" --heap "$E2E_LOAD_HEAP_MB"
pic --stack "$dir_a" data load-genomic --partition synth --vcf-index "$fixture/vcfIndex.tsv" \
	--heap "$E2E_LOAD_HEAP_MB" --promote --enable-profile
check_queries "$a"

say "publish $set_name, then destroy $a"
# Nothing named $set_name existed at the start, so cleanup may remove it.
e2e_sets+=("$set_name")
pic --stack "$dir_a" shared-data publish "$set_name"
pic --stack "$dir_a" destroy --yes
assert_gone "$a"

init_stack "$b" "$dir_b" --hpds-data "shared:$set_name"
say "$b: hydrate the dictionary from the shared set"
pic --stack "$dir_b" dictionary hydrate
check_queries "$b"
if out="$(pic --stack "$dir_b" data load-phenotype --file "$fixture/phenotype.csv" 2>&1)"; then
	fail "$b: load-phenotype into a shared set succeeded"
fi
grep -q 'which is read-only' <<< "$out" || fail "$b: load-phenotype wasn't refused as read-only: $out"

say "destroy $b, then remove $set_name"
pic --stack "$dir_b" destroy --yes
assert_gone "$b"
pic shared-data remove "$set_name"
[ -z "$(docker volume ls -q --filter "name=^${set_name}_")" ] || fail "shared-data remove left volumes of $set_name"

say "e2e genomic passed"
