package netproxy

import (
	"encoding/xml"
	"net/url"
)

type mavenSettings struct {
	XMLName xml.Name     `xml:"http://maven.apache.org/SETTINGS/1.0.0 settings"`
	Proxies []mavenProxy `xml:"proxies>proxy"`
}

type mavenProxy struct {
	ID            string `xml:"id"`
	Active        bool   `xml:"active"`
	Protocol      string `xml:"protocol"`
	Host          string `xml:"host"`
	Port          string `xml:"port"`
	Username      string `xml:"username,omitempty"`
	Password      string `xml:"password,omitempty"`
	NonProxyHosts string `xml:"nonProxyHosts"`
}

// MavenSettings returns a Maven settings.xml whose <proxies> send the
// reactor build's downloads through the proxy, or nil without one. Java
// ignores the proxy env vars, so the reactor container gets this file in
// /root/.m2 instead. It holds the proxy's user and password, so write it
// 0600. Maven, like the JVM, speaks plain HTTP to the proxy whatever its
// URL's scheme.
//
// Maven sends https through an http proxy when it has no https one, so with
// only proxy.http set this is nil too, and Maven's downloads, which are
// https (it blocks external http repositories by default), go direct as
// https does everywhere else with that config.
func (p *Proxy) MavenSettings() []byte {
	if p.https == nil {
		return nil
	}
	nonProxyHosts := p.nonProxyHosts(forMaven)
	var s mavenSettings
	add := func(protocol string, u *url.URL) {
		if u == nil {
			return
		}
		mp := mavenProxy{
			ID: protocol + "-proxy", Active: true, Protocol: protocol,
			Host: u.Hostname(), Port: u.Port(), NonProxyHosts: nonProxyHosts,
		}
		if u.User != nil {
			mp.Username = u.User.Username()
			mp.Password, _ = u.User.Password()
		}
		s.Proxies = append(s.Proxies, mp)
	}
	add("http", p.http)
	add("https", p.https)
	out, err := xml.MarshalIndent(s, "", "  ")
	if err != nil {
		// Strings and a bool always marshal.
		panic(err)
	}
	return append(append([]byte(xml.Header), out...), '\n')
}
