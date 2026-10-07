# pic-sure init

Create a stack in DIR (default: the current directory) and bring it up

Create a PIC-SURE stack in DIR and bring it up: check the host, fetch the
release, write pic-sure.yaml and the secrets, build the images, install the
TLS certificate, render the compose file, set up and migrate the database,
seed it, install the HPDS key and start the services.

Every config flag sets the pic-sure.yaml key its help names, and --set
KEY=VALUE sets any non-secret key, such as --set hpds.java_opts=-Xmx2g.
Secrets are read from stdin only (--auth0-client-secret-stdin,
--db-root-password-stdin; with both, one per line in that order). Ports not
given are 80 and 443, which must be free; --auto-ports takes the first free
pair from 8080/8443 instead. Ports another stack's pic-sure.yaml sets count
as taken. If a port --auto-ports chose, or a dev port, is taken by the time
the stack starts, init chooses again once.

A DIR that already has a pic-sure.yaml is resumed: its config is used as it
is, a config flag, --source or --set that would change it is an error
(change it with pic-sure config set), and the steps already done are
skipped. On a stack init has finished it only registers the stack in
the cache (see cache prune).

```
pic-sure init [DIR] [flags]
```

## Flags

```
      --admin-email string          Email of the first admin user; with Auth0 it must be a Google account. (sets auth.admin_email)
      --auth-mode string            required: no access without login. open: the Discover page without login, with no export or API. explore: the query builder without login; export asks for login. open and explore make the data queryable without login. (sets auth.mode)
      --auth0-client-id string      Auth0 client ID. Evaluation credentials are at avillachlabsupport.hms.harvard.edu. (sets auth.auth0.client_id)
      --auth0-client-secret-stdin   Auth0 client secret, paired with the client ID. Read from stdin. (sets auth.auth0.client_secret)
      --auth0-tenant string         Auth0 tenant. Keep the default unless you run your own tenant. (sets auth.auth0.tenant)
      --auto-ports                  pick free ports from 8080/8443 instead of 80 and 443
      --db-host string              Host name or IP of the external MySQL. (sets db.remote.host)
      --db-mode string              local: the bundled MySQL container. remote: an external MySQL such as RDS. (sets db.mode)
      --db-port string              Port of the external MySQL. (sets db.remote.port)
      --db-root-password-stdin      Password of the external MySQL admin user. Read from stdin. (sets db.remote.root_password)
      --db-root-user string         Admin user on the external MySQL, used to create the PIC-SURE databases and users. (sets db.remote.root_user)
      --hpds-data string            local: this stack's own data. shared: a published shared data set, mounted read-only. init's --hpds-data takes local or shared:NAME. (sets hpds.data)
      --http-port string            Host port for HTTP, which redirects to HTTPS. (sets network.http_port)
      --http-proxy string           Proxy URL for outbound HTTP, such as http://proxy.example.com:3128. Empty for none. (sets proxy.http)
      --https-port string           Host port for HTTPS. Must differ from the HTTP port. (sets network.https_port)
      --https-proxy string          Proxy URL for outbound HTTPS, also http://, since the proxy is spoken to in plain HTTP. Empty for none. (sets proxy.https)
      --ignore-cli-version          go on even if the release was validated with another pic-sure version
      --name string                 Stack name, used as the compose project name and in container and volume names: lowercase letters, digits, - and _, starting with a letter or digit. Fixed once the stack exists. (sets name)
      --no-proxy string             Comma-separated hosts to reach without the proxy. The stack's services, localhost and 127.0.0.1 are always added. (sets proxy.no_proxy)
      --release-branch string       Release-control branch or tag. A tag pins a release; a branch follows it. (sets release.branch)
      --self-update                 if the release needs a newer pic-sure, install it and continue (not with a --*-stdin flag)
      --set KEY=VALUE               set any non-secret config KEY=VALUE, as pic-sure config set does (repeatable)
      --source COMPONENT=PATH       build COMPONENT=PATH from a local checkout (repeatable)
      --theme string                Frontend theme. (sets frontend.theme)
```

The [global flags](pic-sure.md#flags) apply too.

See also: [`pic-sure`](pic-sure.md)
