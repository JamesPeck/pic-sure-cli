// Command genomic-fixture writes the synthetic genomic fixture into DIR.
//
//	go run ./internal/testfixtures/genomic/cmd/genomic-fixture [-abs] DIR
//
// Without -abs, vcfIndex.tsv names the VCFs by bare file name; that is the
// copy checked into testdata/genomic (go generate ./internal/testfixtures/genomic
// refreshes it). With -abs it names them by their absolute path in DIR, the
// form `pic-sure data load-genomic --vcf-index DIR/vcfIndex.tsv --vcf-dir DIR`
// loads.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JamesPeck/pic-sure-cli/internal/testfixtures/genomic"
)

func main() {
	abs := flag.Bool("abs", false, "name the VCFs in vcfIndex.tsv by absolute path")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: genomic-fixture [-abs] DIR")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)
	indexDir := ""
	if *abs {
		var err error
		if dir, err = filepath.Abs(dir); err != nil {
			fmt.Fprintln(os.Stderr, "genomic-fixture:", err)
			os.Exit(1)
		}
		indexDir = dir
	}
	if err := genomic.Write(dir, indexDir); err != nil {
		fmt.Fprintln(os.Stderr, "genomic-fixture:", err)
		os.Exit(1)
	}
}
