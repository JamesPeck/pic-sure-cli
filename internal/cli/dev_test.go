package cli

import (
	"strings"
	"testing"

	"github.com/JamesPeck/pic-sure-cli/internal/catalog"
)

func TestWriteDev(t *testing.T) {
	psama, _ := catalog.LookupDevVariant("psama")
	dict, _ := catalog.LookupDevVariant("dictionary")
	for _, c := range []struct {
		name string
		r    devReport
		v    catalog.DevVariant
		want string
	}{
		{"on with a debug port", devReport{Service: "psama", On: true, Services: []string{"psama"}, Port: 15000, Source: "/src/ps"}, psama,
			"dev psama is on: psama runs from /src/ps; attach a debugger (JDWP) to 127.0.0.1:15000.\n"},
		{"on without one", devReport{Service: "dictionary", On: true, Services: dict.Services, Source: "/src/ps"}, dict,
			"dev dictionary is on: dictionary-api, dictionary-dump run from /src/ps.\n"},
		{"off with the source set", devReport{Service: "psama", Services: []string{"psama"}, Source: "/src/ps"}, psama,
			"dev psama is off: psama has no debug port.\n" +
				"psama still runs the build of components.pic-sure.source (/src/ps), which applies to the whole component.\n" +
				"To return to the release images: pic-sure config set components.pic-sure.source '' && pic-sure up\n"},
	} {
		var b strings.Builder
		if err := writeDev(&b, &c.r, c.v); err != nil {
			t.Fatal(err)
		}
		if b.String() != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, b.String(), c.want)
		}
	}
}
