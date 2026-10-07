// Package genomic generates the synthetic genomic fixture checked into
// testdata/genomic (spec D28): two single-contig VCFs, the vcfIndex.tsv the
// HPDS loaders read, a phenotype CSV for the same patients, and
// expected.json, the patients each documented query must return. All of it
// is invented; none of it comes from a real person or a real reference
// genome. testdata/genomic/README.md records the input format HPDS expects
// and how to load the fixture.
package genomic

//go:generate go run ./cmd/genomic-fixture ../../../testdata/genomic

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Genotypes. HPDS reads exactly three characters per sample when FORMAT is
// GT, so every call is a diploid "a/b".
const (
	het    = "0/1"
	hom    = "1/1"
	homRef = "0/0"
	noCall = "./."
)

// The fixture's file names inside the output directory.
const (
	IndexFile     = "vcfIndex.tsv"
	PhenotypeFile = "phenotype.csv"
	ExpectedFile  = "expected.json"
)

type patient struct {
	id     int    // HPDS patient ID: PATIENT_NUM in the CSV, patient_ids in the index
	sample string // VCF sample column; empty for a phenotype-only patient
	sex    string
	age    int
}

// patients lists everyone in the phenotype CSV. The first eight have
// genomic data, in VCF column order; 109 and 110 have phenotypes only, so a
// query that ignored its genomic filter would count them.
var patients = []patient{
	{101, "SYN_S01", "Female", 34},
	{102, "SYN_S02", "Male", 51},
	{103, "SYN_S03", "Female", 47},
	{104, "SYN_S04", "Male", 29},
	{105, "SYN_S05", "Female", 62},
	{106, "SYN_S06", "Male", 38},
	{107, "SYN_S07", "Female", 55},
	{108, "SYN_S08", "Male", 44},
	{109, "", "Female", 40},
	{110, "", "Male", 33},
}

const (
	sexConcept = `\Genomic Fixture\Sex\`
	ageConcept = `\Genomic Fixture\Age\`
)

type contig struct {
	name   string // CHROM, which HPDS also uses for its directory and in variant specs
	number string // the index's chromosome column
	length int
	file   string
	bgzip  bool
}

// One contig per file, because SplitChromosomeVcfLoader writes each file into
// the directory of the first contig it reads. One file is BGZF and one plain
// so that both values of the index's gzip flag are exercised.
var contigs = []contig{
	{name: "chr21", number: "21", length: 46709983, file: "chr21.vcf.gz", bgzip: true},
	{name: "chr22", number: "22", length: 50818468, file: "chr22.vcf", bgzip: false},
}

type variant struct {
	contig      string
	pos         int
	ref, alt    string
	gene        string
	consequence string
	severity    string
	class       string
	freq        string         // gnomAD allele frequency; empty for a novel variant
	gt          map[int]string // patient ID to genotype; anyone missing is 0/0
}

// variants is the whole data set, sorted by position within each contig as
// the loader requires. Carriers by gene: SYNTHA 102, 103, 104 (104
// homozygous); SYNTHB 105, 106; SYNTHC 103, 107, 108 (107 homozygous);
// SYNTHD 108 (homozygous). Patient 101 carries nothing.
var variants = []variant{
	{"chr21", 33001120, "A", "G", "SYNTHA", "missense_variant", "MODERATE", "SNV", "0.00042", map[int]string{102: het}},
	{"chr21", 33001877, "C", "T", "SYNTHA", "synonymous_variant", "LOW", "SNV", "0.0315", map[int]string{102: het, 103: het, 104: hom}},
	{"chr21", 33002310, "G", "A", "SYNTHA", "stop_gained", "HIGH", "SNV", "", map[int]string{101: noCall, 104: hom}},
	{"chr21", 33003045, "CT", "C", "SYNTHA", "frameshift_variant", "HIGH", "deletion", "0.00008", map[int]string{103: het}},
	{"chr21", 33003900, "T", "C", "SYNTHA", "missense_variant", "MODERATE", "SNV", "0.0021", map[int]string{102: het, 104: het}},
	{"chr21", 33004512, "G", "GA", "SYNTHA", "frameshift_variant", "HIGH", "insertion", "", map[int]string{102: het}},
	{"chr21", 33005260, "A", "C", "SYNTHA", "splice_region_variant", "LOW", "SNV", "0.0123", map[int]string{103: het, 104: hom}},
	{"chr21", 33006033, "C", "G", "SYNTHA", "missense_variant", "MODERATE", "SNV", "0.0007", map[int]string{104: het}},
	{"chr21", 33021400, "T", "A", "SYNTHB", "missense_variant", "MODERATE", "SNV", "0.0011", map[int]string{105: het}},
	{"chr21", 33022150, "G", "T", "SYNTHB", "synonymous_variant", "LOW", "SNV", "0.045", map[int]string{105: het, 106: het}},
	{"chr21", 33023077, "A", "G", "SYNTHB", "missense_variant", "MODERATE", "SNV", "0.0003", map[int]string{106: het}},
	{"chr21", 33024620, "C", "A", "SYNTHB", "stop_gained", "HIGH", "SNV", "", map[int]string{105: het}},
	{"chr21", 33025311, "T", "G", "SYNTHB", "synonymous_variant", "LOW", "SNV", "0.0189", map[int]string{106: het}},
	{"chr21", 33026704, "G", "C", "SYNTHB", "missense_variant", "MODERATE", "SNV", "0.0002", map[int]string{105: het, 106: het}},

	{"chr22", 41010210, "C", "T", "SYNTHC", "missense_variant", "MODERATE", "SNV", "0.0009", map[int]string{103: het}},
	{"chr22", 41011045, "A", "G", "SYNTHC", "synonymous_variant", "LOW", "SNV", "0.0277", map[int]string{103: het, 107: hom, 108: het}},
	{"chr22", 41011890, "G", "A", "SYNTHC", "stop_gained", "HIGH", "SNV", "", map[int]string{101: noCall, 107: hom}},
	{"chr22", 41012433, "T", "C", "SYNTHC", "missense_variant", "MODERATE", "SNV", "0.0016", map[int]string{108: het}},
	{"chr22", 41013720, "AG", "A", "SYNTHC", "frameshift_variant", "HIGH", "deletion", "", map[int]string{107: het}},
	{"chr22", 41014388, "C", "A", "SYNTHC", "missense_variant", "MODERATE", "SNV", "0.0005", map[int]string{103: het, 106: noCall, 108: het}},
	{"chr22", 41015061, "G", "T", "SYNTHC", "splice_region_variant", "LOW", "SNV", "0.0102", map[int]string{107: hom}},
	{"chr22", 41016900, "A", "T", "SYNTHC", "missense_variant", "MODERATE", "SNV", "0.0031", map[int]string{108: het}},
	{"chr22", 41030140, "T", "C", "SYNTHD", "missense_variant", "MODERATE", "SNV", "0.0004", map[int]string{108: hom}},
	{"chr22", 41031555, "C", "CT", "SYNTHD", "frameshift_variant", "HIGH", "insertion", "", map[int]string{108: het}},
	{"chr22", 41032780, "G", "A", "SYNTHD", "synonymous_variant", "LOW", "SNV", "0.0601", map[int]string{108: hom}},
	{"chr22", 41033415, "A", "G", "SYNTHD", "stop_gained", "HIGH", "SNV", "", map[int]string{108: het}},
}

// infoFields describes the INFO keys, in the order they appear on a record.
// They are the annotation columns PIC-SURE's genomic filters use. HPDS
// takes each ##INFO line's 4th comma-separated part as the description and
// drops every '>', so keep ID, Number, Type, Description in that order and
// keep '>' out of descriptions.
var infoFields = []struct{ id, typ, desc string }{
	{"Gene_with_variant", "String", "Official symbol of the gene the variant affects (synthetic)"},
	{"Variant_severity", "String", "Severity of the calculated consequence: HIGH, MODERATE or LOW"},
	{"Variant_consequence_calculated", "String", "Sequence Ontology term for the calculated consequence"},
	{"Variant_class", "String", "Variant type: SNV, deletion or insertion"},
	{"Variant_frequency_in_gnomAD", "Float", "Allele frequency in gnomAD (synthetic)"},
	{"Variant_frequency_as_text", "String", "Frequency category: Novel, Rare (below 1%) or Common"},
}

// info returns the variant's INFO values keyed by field ID. Every value is a
// key=value pair: HPDS pairs up the INFO tokens, so a bare flag would shift
// every value after it.
func (v variant) info() map[string]string {
	m := map[string]string{
		"Gene_with_variant":              v.gene,
		"Variant_severity":               v.severity,
		"Variant_consequence_calculated": v.consequence,
		"Variant_class":                  v.class,
		"Variant_frequency_as_text":      frequencyText(v.freq),
	}
	if v.freq != "" {
		m["Variant_frequency_in_gnomAD"] = v.freq
	}
	return m
}

func frequencyText(freq string) string {
	if freq == "" {
		return "Novel"
	}
	f, err := strconv.ParseFloat(freq, 64)
	if err != nil {
		panic(fmt.Sprintf("variant frequency %q: %v", freq, err))
	}
	if f < 0.01 {
		return "Rare"
	}
	return "Common"
}

func (v variant) genotype(patientID int) string {
	if g, ok := v.gt[patientID]; ok {
		return g
	}
	return homRef
}

// spec is the variant's HPDS spec without gene and consequence, the form a
// query's genomic filter key takes. HPDS matches it as a prefix of the
// stored spec.
func (v variant) spec() string {
	return fmt.Sprintf("%s,%d,%s,%s", v.contig, v.pos, v.ref, v.alt)
}

func genomicPatients() []patient {
	var out []patient
	for _, p := range patients {
		if p.sample != "" {
			out = append(out, p)
		}
	}
	return out
}

// Write writes the fixture into dir, creating it if needed. vcfIndex.tsv
// names each VCF as indexDir joined with its file name. The loader opens
// those paths inside its container, so pass the directory as the container
// sees it, or "" for bare file names (the checked-in copy).
func Write(dir, indexDir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		IndexFile:     vcfIndex(indexDir),
		PhenotypeFile: phenotypeCSV(),
	}
	exp, err := expectedJSON()
	if err != nil {
		return err
	}
	files[ExpectedFile] = exp
	for _, c := range contigs {
		data := vcf(c)
		if c.bgzip {
			data = bgzf(data)
		}
		files[c.file] = data
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// vcf renders one contig's VCF.
func vcf(c contig) []byte {
	var b strings.Builder
	b.WriteString("##fileformat=VCFv4.2\n")
	b.WriteString("##source=pic-sure-cli synthetic genomic fixture; not real data\n")
	fmt.Fprintf(&b, "##contig=<ID=%s,length=%d>\n", c.name, c.length)
	b.WriteString("##FILTER=<ID=PASS,Description=\"All filters passed\">\n")
	b.WriteString("##FORMAT=<ID=GT,Number=1,Type=String,Description=\"Genotype\">\n")
	for _, f := range infoFields {
		fmt.Fprintf(&b, "##INFO=<ID=%s,Number=1,Type=%s,Description=\"%s\">\n", f.id, f.typ, f.desc)
	}
	b.WriteString("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT")
	gp := genomicPatients()
	for _, p := range gp {
		b.WriteString("\t" + p.sample)
	}
	b.WriteString("\n")
	for _, v := range variants {
		if v.contig != c.name {
			continue
		}
		info := v.info()
		var pairs []string
		for _, f := range infoFields {
			if val, ok := info[f.id]; ok {
				pairs = append(pairs, f.id+"="+val)
			}
		}
		fmt.Fprintf(&b, "%s\t%d\t.\t%s\t%s\t.\tPASS\t%s\tGT", v.contig, v.pos, v.ref, v.alt, strings.Join(pairs, ";"))
		for _, p := range gp {
			b.WriteString("\t" + v.genotype(p.id))
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// vcfIndex renders vcfIndex.tsv. HPDS skips the first line as a header and
// reads six columns: file, chromosome, annotated, gzip, sample IDs and
// patient IDs. Sample and patient IDs pair up by position.
func vcfIndex(indexDir string) []byte {
	var b strings.Builder
	b.WriteString("filename\tchromosome\tannotated\tgzip\tsample_ids\tpatient_ids\tsample_relationship\trelated_sample_ids\n")
	var samples, ids []string
	for _, p := range genomicPatients() {
		samples = append(samples, p.sample)
		ids = append(ids, strconv.Itoa(p.id))
	}
	for _, c := range contigs {
		file := c.file
		if indexDir != "" {
			file = path.Join(filepath.ToSlash(indexDir), c.file)
		}
		gzip := "0"
		if c.bgzip {
			gzip = "1"
		}
		fmt.Fprintf(&b, "%s\t%s\t1\t%s\t%s\t%s\n", file, c.number, gzip, strings.Join(samples, ","), strings.Join(ids, ","))
	}
	return []byte(b.String())
}

// phenotypeCSV renders the CSV that CSVLoaderNewSearch reads. Rows are
// grouped by concept path, because the loader builds one concept at a time.
func phenotypeCSV() []byte {
	var b strings.Builder
	b.WriteString("PATIENT_NUM,CONCEPT_PATH,NVAL_NUM,TVAL_CHAR\n")
	for _, p := range patients {
		fmt.Fprintf(&b, "%d,%s,%d,\n", p.id, ageConcept, p.age)
	}
	for _, p := range patients {
		fmt.Fprintf(&b, "%d,%s,,%s\n", p.id, sexConcept, p.sex)
	}
	return []byte(b.String())
}

// filter is one genomic filter, shaped like an entry of a PIC-SURE query's
// genomicFilters. Key is an INFO column or a variant spec
// ("chr21,33001877,C,T"); for a spec, Values are zygosities ("0/1", "1/1").
type filter struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// phenotypeFilter keeps patients whose value for ConceptPath is one of
// Values.
type phenotypeFilter struct {
	ConceptPath string   `json:"conceptPath"`
	Values      []string `json:"values"`
}

// query is one documented query and the patients it must return.
type query struct {
	Name             string            `json:"name"`
	GenomicFilters   []filter          `json:"genomicFilters"`
	PhenotypeFilters []phenotypeFilter `json:"phenotypeFilters,omitempty"`
	Patients         []int             `json:"patients"`
}

// expectations is the content of expected.json.
type expectations struct {
	GenomicPatients   []int   `json:"genomicPatients"`
	PhenotypePatients []int   `json:"phenotypePatients"`
	Queries           []query `json:"queries"`
}

// queries are the documented queries; Patients is filled in by evaluate.
var queries = []query{
	{Name: "gene-SYNTHA", GenomicFilters: []filter{{"Gene_with_variant", []string{"SYNTHA"}}}},
	{Name: "gene-SYNTHB", GenomicFilters: []filter{{"Gene_with_variant", []string{"SYNTHB"}}}},
	{Name: "gene-SYNTHC", GenomicFilters: []filter{{"Gene_with_variant", []string{"SYNTHC"}}}},
	{Name: "gene-SYNTHD", GenomicFilters: []filter{{"Gene_with_variant", []string{"SYNTHD"}}}},
	{Name: "gene-SYNTHA-or-SYNTHD", GenomicFilters: []filter{{"Gene_with_variant", []string{"SYNTHA", "SYNTHD"}}}},
	{Name: "gene-unknown", GenomicFilters: []filter{{"Gene_with_variant", []string{"NOSUCHGENE"}}}},
	{Name: "consequence-stop_gained", GenomicFilters: []filter{{"Variant_consequence_calculated", []string{"stop_gained"}}}},
	{Name: "frequency-Novel", GenomicFilters: []filter{{"Variant_frequency_as_text", []string{"Novel"}}}},
	{Name: "gene-SYNTHA-and-stop_gained", GenomicFilters: []filter{
		{"Gene_with_variant", []string{"SYNTHA"}},
		{"Variant_consequence_calculated", []string{"stop_gained"}},
	}},
	{Name: "variant-chr21-33001877-het", GenomicFilters: []filter{{"chr21,33001877,C,T", []string{het}}}},
	{Name: "variant-chr21-33001877-hom", GenomicFilters: []filter{{"chr21,33001877,C,T", []string{hom}}}},
	{Name: "variant-chr21-33001877-any", GenomicFilters: []filter{{"chr21,33001877,C,T", []string{het, hom}}}},
	{Name: "gene-SYNTHC-female", GenomicFilters: []filter{{"Gene_with_variant", []string{"SYNTHC"}}},
		PhenotypeFilters: []phenotypeFilter{{sexConcept, []string{"Female"}}}},
}

// evaluate returns the patients q matches, with HPDS's semantics. INFO
// filters select the variants that match all of them, and a patient
// matches when heterozygous or homozygous for any selected variant. Each
// variant-spec filter keeps the patients whose genotype for that variant is
// one of its zygosities. Phenotype filters intersect with the result.
func (q query) evaluate() []int {
	var infoFilters, specFilters []filter
	for _, f := range q.GenomicFilters {
		if strings.Count(f.Key, ",") >= 3 {
			specFilters = append(specFilters, f)
		} else {
			infoFilters = append(infoFilters, f)
		}
	}
	matched := []int{}
	for _, p := range genomicPatients() {
		if q.matches(p, infoFilters, specFilters) {
			matched = append(matched, p.id)
		}
	}
	return matched
}

func (q query) matches(p patient, infoFilters, specFilters []filter) bool {
	if len(infoFilters) > 0 {
		carrier := false
		for _, v := range variants {
			if variantMatches(v, infoFilters) && isCarrier(v.genotype(p.id)) {
				carrier = true
				break
			}
		}
		if !carrier {
			return false
		}
	}
	for _, f := range specFilters {
		found := false
		for _, v := range variants {
			if strings.HasPrefix(v.spec(), f.Key) && slices.Contains(f.Values, v.genotype(p.id)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, f := range q.PhenotypeFilters {
		if !slices.Contains(f.Values, phenotype(p, f.ConceptPath)) {
			return false
		}
	}
	return true
}

func variantMatches(v variant, filters []filter) bool {
	info := v.info()
	for _, f := range filters {
		val, ok := info[f.Key]
		if !ok || !slices.Contains(f.Values, val) {
			return false
		}
	}
	return true
}

func isCarrier(gt string) bool { return gt == het || gt == hom }

func phenotype(p patient, concept string) string {
	switch concept {
	case sexConcept:
		return p.sex
	case ageConcept:
		return strconv.Itoa(p.age)
	}
	panic("unknown concept " + concept)
}

func expected() expectations {
	e := expectations{}
	for _, p := range patients {
		e.PhenotypePatients = append(e.PhenotypePatients, p.id)
	}
	for _, p := range genomicPatients() {
		e.GenomicPatients = append(e.GenomicPatients, p.id)
	}
	for _, q := range queries {
		q.Patients = q.evaluate()
		e.Queries = append(e.Queries, q)
	}
	return e
}

func expectedJSON() ([]byte, error) {
	data, err := json.MarshalIndent(expected(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// bgzfBlockData is how much input goes into each BGZF block, as in bgzip.
const bgzfBlockData = 0xff00

// bgzfEOF is the empty block that ends every BGZF file (SAM spec §4.1.2).
var bgzfEOF = []byte{
	0x1f, 0x8b, 0x08, 0x04, 0, 0, 0, 0, 0, 0xff, 0x06, 0x00, 'B', 'C', 0x02, 0x00,
	0x1b, 0x00, 0x03, 0x00, 0, 0, 0, 0, 0, 0, 0, 0,
}

// bgzf encodes data as BGZF, the blocked gzip that bgzip writes. HPDS reads
// a VCF flagged gzip=1 with htsjdk's BlockCompressedInputStream, which
// accepts only BGZF, not plain gzip. The blocks hold stored (uncompressed)
// deflate data, so the output never changes with the Go version.
func bgzf(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		n := min(len(data), bgzfBlockData)
		writeBGZFBlock(&out, data[:n])
		data = data[n:]
	}
	out.Write(bgzfEOF)
	return out.Bytes()
}

// writeBGZFBlock writes one gzip member whose extra field carries BGZF's BC
// subfield: the total block size minus one.
func writeBGZFBlock(out *bytes.Buffer, data []byte) {
	const overhead = 18 + 5 + 8 // gzip header with BC subfield, stored-block header, CRC32 and ISIZE
	b := []byte{0x1f, 0x8b, 0x08, 0x04, 0, 0, 0, 0, 0, 0xff, 0x06, 0x00, 'B', 'C', 0x02, 0x00}
	b = binary.LittleEndian.AppendUint16(b, uint16(len(data)+overhead-1))
	b = append(b, 0x01) // final block, stored
	b = binary.LittleEndian.AppendUint16(b, uint16(len(data)))
	b = binary.LittleEndian.AppendUint16(b, ^uint16(len(data)))
	b = append(b, data...)
	b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(data))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	out.Write(b)
}
