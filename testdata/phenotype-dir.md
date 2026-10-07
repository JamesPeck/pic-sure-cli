# Synthetic multi-file phenotype fixture

`phenotype-dir/` is an input directory for
`pic-sure data load-phenotype --input-dir testdata/phenotype-dir`: three
allConcepts-format CSVs about 20 made-up patients under `\Synthetic Multi\`,
no real data.

- `demographics.csv`: `Age` (numeric) and `Sex` (Female for even patient
  numbers, Male for odd).
- `labs.csv`: `Glucose` (numeric).
- `lifestyle.csv`: `Smoker` (Yes for every third patient, else No).

Generated with:

```sh
cd testdata/phenotype-dir
h='PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR,TIMESTAMP'
t=1704067200000
{ echo "$h"; for i in $(seq 1 20); do
    echo "$i,\\Synthetic Multi\\Demographics\\Age\\,$((20 + i * 3)),,$t"
    if [ $((i % 2)) = 0 ]; then s=Female; else s=Male; fi
    echo "$i,\\Synthetic Multi\\Demographics\\Sex\\,,$s,$t"
  done; } > demographics.csv
{ echo "$h"; for i in $(seq 1 20); do
    echo "$i,\\Synthetic Multi\\Labs\\Glucose\\,$((80 + i * 2)),,$t"
  done; } > labs.csv
{ echo "$h"; for i in $(seq 1 20); do
    if [ $((i % 3)) = 0 ]; then s=Yes; else s=No; fi
    echo "$i,\\Synthetic Multi\\Lifestyle\\Smoker\\,,$s,$t"
  done; } > lifestyle.csv
```
