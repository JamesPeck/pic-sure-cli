#!/usr/bin/env bash
# The directory loader and a custom dictionary (spec §9.6, §9.7, §11): init →
# data load-phenotype --input-dir testdata/phenotype-dir (three CSVs) → HPDS
# answers counts that need rows from each file → a --dictionary custom load
# with AIO's older facet headers is refused before HPDS is touched → the same
# load of testdata/dictionary succeeds, and the dictionary API finds its
# concepts and facets → destroy. Needs docker, git, go and jq; settings are
# in scripts/e2e-lib.sh.

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-input-dir}"
dir="$E2E_WORK/$name"
input="$repo_root/testdata/phenotype-dir"
dict="$repo_root/testdata/dictionary"

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

# dictionary PATH BODY posts BODY to the stack's dictionary API from a
# container on its app network and prints the answer.
dictionary() {
	docker run --rm --network "${name}_app" "$helper" \
		wget -q -O - --header 'Content-Type: application/json' \
		--post-data "$2" "http://dictionary-api/$1" < /dev/null
}

# custom_load FACET_CATEGORIES FACETS loads testdata/dictionary with these
# facet files.
custom_load() {
	pic --stack "$dir" data load-phenotype --file "$dict/allConcepts.csv" --heap "$E2E_LOAD_HEAP_MB" \
		--dictionary custom --datasets "$dict/datasets.csv" --concepts "$dict/concepts.zip" \
		--facets-categories "$1" --facets "$2" --facet-concepts "$dict/facet_concepts.csv"
}

# no_etl fails if a dictionary-etl container of the stack is left.
no_etl() {
	local left
	left="$(docker ps -aq --filter "name=^${name}-dictionaryetl-")"
	[ -z "$left" ] || fail "$1 left dictionary-etl containers: $(echo "$left" | tr '\n' ' ')"
}

init_stack "$name" "$dir"

say "data load-phenotype --input-dir"
pic --stack "$dir" data load-phenotype --input-dir "$input" --heap "$E2E_LOAD_HEAP_MB"
deep_status "$dir" '.deep.data.ready == true' > /dev/null || fail "status --deep: want data ready"

say "HPDS has every file's rows"
expect demographics.csv "\\Synthetic Multi\\Demographics\\Sex\\" Female
expect lifestyle.csv "\\Synthetic Multi\\Lifestyle\\Smoker\\" Yes
want="$(awk -F, 'NR > 1 && $3 >= 100 { n[$1] = 1 } END { print length(n) }' "$input/labs.csv")"
got="$(count '{"conceptPath": "\\Synthetic Multi\\Labs\\Glucose\\", "min": 100}')"
[ "$got" = "$want" ] || fail "Glucose >= 100: HPDS counts $got patients, labs.csv has $want"
echo "  \\Synthetic Multi\\Labs\\Glucose\\ >= 100: $got patients" >&2

say "--dictionary custom with AIO's older facet headers is refused first"
old_facets="$E2E_WORK/aio-facets"
mkdir -p "$old_facets"
printf '%s\n' '"name","display name","description"' '"domain","Domain","Synthetic fixture category"' \
	> "$old_facets/facet_categories.csv"
printf '%s\n' '"facet_category","facet_name","display_name","description","parent_name"' \
	'"domain","demographics","Demographics","Demographics concepts",""' > "$old_facets/facets.csv"
started="$(docker inspect -f '{{.State.StartedAt}}' "$(container "$name" hpds)")"
rc=0
out="$(custom_load "$old_facets/facet_categories.csv" "$old_facets/facets.csv" 2>&1)" || rc=$?
[ "$rc" -eq 2 ] || fail "old facet headers: exit $rc, want 2: $out"
grep -qF 'lacks the name(unique)' <<< "$out" || fail "old facet headers: the categories file wasn't refused for name(unique): $out"
[ "$(docker inspect -f '{{.State.StartedAt}}' "$(container "$name" hpds)")" = "$started" ] ||
	fail "old facet headers: HPDS was restarted"
no_etl "the refused load"

say "--dictionary custom with testdata/dictionary"
custom_load "$dict/facet_categories.csv" "$dict/facets.csv"
no_etl "the custom load"
smoker="\\Synthetic Custom\\Lifestyle\\Smoker\\"
got="$(dictionary concepts '{"search": "Smoker"}' | jq -r '.content[].conceptPath')"
grep -qxF "$smoker" <<< "$got" || fail "searching the dictionary for Smoker found: $got"
got="$(dictionary facets '{}' |
	jq -r '.[] | select(.name == "synthetic_domain") | .facets[] | "\(.name) \(.count)"')"
[ "$got" = "synthetic_lifestyle 1" ] || fail "the dictionary's synthetic_domain facets: $got"
got="$(dictionary concepts '{"facets": [{"name": "synthetic_lifestyle", "category": "synthetic_domain"}]}' |
	jq -r '.content[].conceptPath')"
[ "$got" = "$smoker" ] || fail "the synthetic_lifestyle facet's concepts: $got"
echo "  dictionary: $smoker, facet synthetic_domain/synthetic_lifestyle" >&2

say "destroy"
pic --stack "$dir" destroy --yes
assert_gone "$name"

say "e2e input-dir passed"
