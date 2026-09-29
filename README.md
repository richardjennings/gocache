# gocache

A shared Go build cache for CI builders on one host.

Since Go 1.24, the go command can use an external build cache: it starts the
command that `GOCACHEPROG` names and asks it for outputs over stdin and
stdout. `gocache client` is that command. It gets outputs from
`gocache server`, a small HTTP server that keeps the cache on disk. Builders
that start empty, such as a Docker build in CI, then reuse the packages and
test results that other builders compiled.

```
go ──(GOCACHEPROG protocol: JSON on stdin/stdout)──▶ gocache client ──(HTTP)──▶ gocache server
```

## Use in a Dockerfile

```dockerfile
RUN go install github.com/richardjennings/gocache@v0.1.0
ARG GOCACHE_SERVER
ENV GOCACHE_SERVER=$GOCACHE_SERVER GOCACHEPROG="${GOCACHE_SERVER:+/go/bin/gocache client}"
```

In CI, pass `--build-arg GOCACHE_SERVER=http://HOST:5100`, where `HOST` is an
address of the server that the build can reach. A build without the argument
leaves `GOCACHEPROG` empty, and go uses its normal cache.

The `ENV` values stay in every image built from this stage. Use them in a
build or test stage, not in a stage that you ship.

## Behaviour

- A get that cannot reach the server is a miss, and the go command builds the
  output itself. A server problem never fails a build. After the first
  connection error or timeout, the client stops asking the server. So a
  server that is down or stuck delays a go command once, not once per
  request.
- The client writes outputs to a temporary directory, and removes it before
  it exits. So the outputs do not end up in an image layer.
- The go command names each output by the SHA-256 of its bytes. The server
  refuses an upload that does not match its name, and the client refuses a
  download that does not match.
- The client reports the first server error on stderr when it exits.

## Limitations

- `go list -export` does not work with gocache. The go command sends no
  `close` after `go list`, and it does not wait for the client. The client
  removes the export files shortly after `go list` exits, so tools that read
  them, such as staticcheck, fail.
- `go list -compiled` does not work with gocache. With Go 1.25 it fails with
  "loading compiled Go files from cache: reading srcfiles list: cache entry
  not found". Tools that ask `golang.org/x/tools/go/packages` for compiled
  files, such as deadcode, use it. Run such tools with an empty `GOCACHEPROG`.
- The go command does not store linked binaries in a `GOCACHEPROG` cache. So
  a builder that starts empty links each main package again. `go test` does
  not link when it finds the test result in the cache.

## Server

```
gocache server -addr HOST:5100 -dir /var/lib/gocache -max-bytes 53687091200 -sweep 10m
```

It has no authentication, so give `-addr` an address that only the builders
can reach. Any build that can reach the server can store outputs that later
builds use. So do not share one server between builds of untrusted code, such
as pull requests from forks, and builds that you release.

Every `-sweep` interval, if the stored files total more than
`-max-bytes`, it removes the least recently used files until they total at most
90% of `-max-bytes`.

| Request | Meaning |
|---|---|
| `GET /a/{actionID}` | the output ID, size and time stored for an action, or 404 |
| `PUT /a/{actionID}` | store them; the output must be stored first |
| `GET /o/{outputID}` | the output bytes, or 404 |
| `PUT /o/{outputID}` | store output bytes; they must match the ID |
| `GET /stats` | hit, miss and upload counters |

IDs are the 64-character lowercase hex form of the go command's IDs.

## Deploy on one host

These steps use systemd and the unit file in `deploy/`. systemd creates a user
for the server and its data directory, `/var/lib/gocache`.

1. Build gocache for the host:
   `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o gocache .`
2. Copy `gocache` to `/usr/local/bin/` on the host.
3. Copy `deploy/gocache.service` to `/etc/systemd/system/`.
4. Set the listen address: `sudo systemctl edit gocache`, and add
   `Environment=GOCACHE_ADDR=HOST:5100` under `[Service]`. If another service
   creates `HOST`, also add `After=` and `Requires=` for that service under
   `[Unit]`, as the comments in the unit file show.
5. Start it: `sudo systemctl enable --now gocache`.
6. Check it: `curl http://HOST:5100/stats`.
