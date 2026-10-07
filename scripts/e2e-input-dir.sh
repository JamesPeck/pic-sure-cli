#!/usr/bin/env bash
# The directory loader (spec §9.6, §11): init → data load-phenotype
# --input-dir testdata/phenotype-dir (three CSVs) → HPDS answers counts that
# need rows from each file → destroy. Needs docker, git, go and jq; settings
# are in scripts/e2e-lib.sh.

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-input-dir}"
dir="$E2E_WORK/$name"
input="$repo_root/testdata/phenotype-dir"

# count FILTER prints how many patients match HPDS phenotypic FILTER (JSON).
count() {
	hpds "$name" query/sync "$(jq -c '{query: {select: [], genomicFilters: [],
		phenotypicClause: (. + {phenotypicFilterType: "FILTER", not: false}), expectedResultType: "COUNT"}}' <<< "$1")"
}

# expect FILE CONCEPT VALUE compares HPDS's count of patients with VALUE
# for the categorical CONCEPT with the fixture's.
expect() {
	local want got
	want="$(P="$2" V="$3" awk -F, '$2 == ENVIRON["P"] && $4 == ENVIRON["V"] { n[$1] = 1 } END { print length(n) }' "$input/$1")"
	got="$(count "$(jq -cn --arg p "$2" --arg v "$3" '{conceptPath: $p, values: [$v]}')")"
	[ "$got" = "$want" ] || fail "$2 = $3: HPDS counts $got patients, $1 has $want"
	echo "  $2 = $3: $got patients" >&2
}

init_stack "$name" "$dir"

say "data load-phenotype --input-dir"
pic --stack "$dir" data load-phenotype --input-dir "$input" --heap "$E2E_LOAD_HEAP_MB"
deep_status "$dir" '.deep.data.ready == true' > /dev/null || fail "status --deep: want data ready"

say "HPDS has every file's rows"
expect demographics.csv "\\Synthetic Multi\\Demographics\\Sex\\" Female
expect lifestyle.csv "\\Synthetic Multi\\Lifestyle\\Smoker\\" Yes
# Glucose is numeric.
want="$(awk -F, 'NR > 1 && $3 >= 100 { n[$1] = 1 } END { print length(n) }' "$input/labs.csv")"
got="$(count '{"conceptPath": "\\Synthetic Multi\\Labs\\Glucose\\", "min": 100}')"
[ "$got" = "$want" ] || fail "Glucose >= 100: HPDS counts $got patients, labs.csv has $want"
echo "  \\Synthetic Multi\\Labs\\Glucose\\ >= 100: $got patients" >&2

say "destroy"
pic --stack "$dir" destroy --yes
assert_gone "$name"

say "e2e input-dir passed"
