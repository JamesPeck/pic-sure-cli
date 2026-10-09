// Command maven-settings prints the settings.xml the reactor build mounts
// for a proxy (netproxy.MavenSettings), for scripts/e2e-proxy.sh.
//
//	go run ./internal/testfixtures/maven-settings -http URL -https URL [-no-proxy LIST]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/JamesPeck/pic-sure-cli/internal/netproxy"
)

func main() {
	var c netproxy.Config
	flag.StringVar(&c.HTTP, "http", "", "proxy.http")
	flag.StringVar(&c.HTTPS, "https", "", "proxy.https")
	flag.StringVar(&c.NoProxy, "no-proxy", "", "proxy.no_proxy")
	flag.Parse()
	p, err := netproxy.New(c, netproxy.CatalogServices())
	if err != nil {
		fmt.Fprintln(os.Stderr, "maven-settings:", err)
		os.Exit(2)
	}
	s := p.MavenSettings()
	if s == nil {
		fmt.Fprintln(os.Stderr, "maven-settings: Maven needs an https proxy")
		os.Exit(2)
	}
	if _, err := os.Stdout.Write(s); err != nil {
		os.Exit(1)
	}
}
