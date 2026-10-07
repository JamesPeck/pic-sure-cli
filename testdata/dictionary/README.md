# Synthetic custom-dictionary fixture

Synthetic data for `pic-sure data load-phenotype --dictionary custom`; no
real patients. `allConcepts.csv` holds 30 made-up patients under
`\Synthetic Custom\`, and the other files describe the same concepts:

```sh
pic-sure data load-phenotype --file allConcepts.csv --dictionary custom \
  --datasets datasets.csv --concepts concepts.zip \
  --facets-categories facet_categories.csv --facets facets.csv \
  --facet-concepts facet_concepts.csv
```

`concepts.zip` holds `concepts_0.csv`. The facet files use the headers
dictionary-etl c97a813 requires (`name(unique)`, `facet_name(unique)`),
which AIO's `fixtures/etl/custom` predates.
