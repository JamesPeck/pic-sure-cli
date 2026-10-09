# Testing the proxy

`scripts/e2e-proxy.sh` checks that a stack's `proxy` block (spec §9.10)
reaches every egress path. It runs a squid proxy, creates a stack whose
proxy is that squid, and uses squid's access log as the evidence: each check
records the log's length, runs one path, then waits for a successful
`CONNECT host:443` logged after that point.

```bash
scripts/e2e-proxy.sh                 # builds pic-sure from this checkout
E2E_NAME=my-proxy-test scripts/e2e-proxy.sh
```

It needs docker, git, go, jq and curl, and takes about 3 minutes when the
stack's images are already built. It sources `scripts/e2e-lib.sh`, so the
`E2E_*` settings documented there apply (stack name, heap sizes, artifacts
on failure, `E2E_KEEP`). Its own settings are at the top of the script.
If `<name>-squid`, `<name>-egress` or `<name>-internal` already exists
(another run's, say), creating it fails the run, and cleanup removes only
what this run created.

## Setup

- **squid** (`<name>-squid`, `ubuntu/squid` with its default config, which
  allows the private ranges) runs on its own bridge network
  (`<name>-egress`), which has internet access. Its port 3128 is published
  on a random port of the proxy address only (below). squid lets any
  private-range client in, so on macOS, where that address is the LAN
  address, other machines on the LAN can use it while the script runs.
- **An internal network** (`<name>-internal`, `docker network create
  --internal`) has no route out. squid is also attached to it, under the
  alias `squid`, so a container there can get out only through squid. The
  script first checks that a container on it can't reach the internet.
- **The proxy URL** is `http://<E2E_PROXY_HOST>:<port>`. It has to work
  both from this host (git, the CLI's own HTTP) and from containers (psama,
  docker builds), so it can't be `localhost`. The default is the default
  bridge's gateway on Linux and the Mac's LAN address (`en0`, else `en1`)
  with Docker Desktop, whose VM reaches it.
- **A fresh cache.** `XDG_CACHE_HOME` points at a new directory under
  `~/.cache`, so init really clones the release and the sources, and demo
  really downloads, instead of reusing the user's cache. Docker images and
  the `pic-sure-m2` volume belong to the daemon, so a stack whose images
  already exist doesn't rebuild them.
- The stack is `init --auth-mode open --auto-ports --http-proxy URL
  --https-proxy URL`. Open mode needs no Auth0 login, so the run needs no
  Auth0-registered port.

Everything is removed at the end: the stack (`destroy`), squid, both
networks and the cache directory. With `E2E_KEEP=1`, a failed run leaves
all of it for debugging.

## What each check proves

| Path (§9.10) | Check | Evidence |
|---|---|---|
| `git` subprocesses | `init` clones release-control and every component | CONNECT `github.com` |
| `doctor --network` | doctor passes; its probes and its `git ls-remote` use the proxy | CONNECT `github.com`, `repo.maven.apache.org`, `registry.npmjs.org`, `dl-cdn.alpinelinux.org` |
| CLI's own HTTP | `data demo nhanes` downloads and loads | CONNECT `raw.githubusercontent.com` |
| CLI's own HTTP | `self-update --to 0.0.1`: the release lookup, which then fails (nothing is replaced) | CONNECT `api.github.com` |
| JVM runtime (psama → Auth0) | `java` in the psama container, with only the proxy properties from its rendered `JAVA_OPTS`, GETs the tenant's `/.well-known/openid-configuration` | CONNECT `<tenant>.auth0.com` |
| Maven reactor | `mvn dependency:get` of one artifact into an empty local repo, on the internal network, with the reactor's `settings.xml`; the same command without it must fail | CONNECT `repo.maven.apache.org` |
| `docker build` (apk, npm/pnpm) | a probe image from the base image of the stack's frontend Dockerfile runs `apk add pnpm`, `pnpm add` and `npm view`, built `--no-cache` with the stack's proxy variables as `--build-arg` | CONNECT `dl-cdn.alpinelinux.org`, `registry.npmjs.org` |
| Image pulls (D36) | doctor's test pull fails, and its detail gives the daemon proxy instructions | `network-docker-pull` is `fail`, and its detail names the proxy URL (and, on Docker Desktop, its settings page) |
| no-proxy list | squid denied nothing, and saw no request for a single-label host (the stack's services, `localhost`) or `127.*` | the whole log |

### Why the small probes are enough

A full image build through squid from an empty Maven repo takes a long time
and proves nothing more, because each path's proxy wiring is one mechanism
that a single download exercises:

- **Maven.** The reactor build writes `netproxy.MavenSettings()` to a
  temporary file, mounts it and runs `mvn -s` with it. The script gets the
  same bytes from `go run ./internal/testfixtures/maven-settings`, for the
  proxy `http://squid:3128` (the internal network can't reach the published
  port). From an empty repo,
  `dependency:get` downloads a few hundred artifacts (the plugin and its
  dependencies), all through squid, and fails without the settings.
- **docker build.** The image builds pass `netproxy.BuildArgs()` as bare
  `--build-arg NAME` with the values in the docker CLI's environment; the
  script does the same with the stack's proxy variables. Build args aren't
  part of BuildKit's cache key, so a real image rebuild with cached layers
  would download nothing; `--no-cache` on a small probe does. The probe runs
  the frontend Dockerfile's own package steps on its base image.
  BuildKit's builds run on the daemon's network, which has direct egress,
  so here the evidence is the log entries rather than isolation.
- **psama.** psama in open mode makes no Auth0 calls, so the script runs
  the JVM's own `ProxySelector` with psama's rendered proxy properties in
  psama's container. psama logs `Utilizing unauthenticated proxy:
  host=<proxy host>` at startup, which shows its REST client also picks
  them up.
- **Daemon pulls (D36).** The CLI can't route the daemon's pulls, and the
  daemon's settings must not be changed for a test. The script puts a
  `docker` shim first on `PATH` that sends `docker pull REF` to
  `<name>.invalid/REF`, so the real daemon fails the pull with the error it
  gives when it can't get out, and doctor reacts to that.

## Findings

The first run found two gaps, fixed with this setup:

- psama never became healthy. busybox `wget` ignores `no_proxy`, so its
  healthcheck sent `http://127.0.0.1:8090/...` to squid, which refused it.
  The psama and httpd-hmr healthchecks now pass `-Y off`.
- Docker Desktop's `docker info` always reports its internal forwarding
  proxy (`http.docker.internal:3128`), which doctor printed as "the
  daemon's proxy". doctor now says what that proxy is, and the Docker
  Desktop instructions name the URLs and the bypass list to enter.

## Limits

- Image pulls go direct in this setup: Docker Desktop's VM and a Linux
  daemon have their own egress, and only the daemon's own proxy settings
  could change that (D36).
- Credentialed proxy URLs aren't covered; JVM runtime services can't use
  them (§9.10). squid here needs no authentication.
- The host itself isn't firewalled, so for the CLI's own paths (git, HTTP)
  the evidence is squid's log, not isolation.
