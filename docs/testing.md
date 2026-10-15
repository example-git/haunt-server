# Testing and platform coverage

`go test ./...` covers the service's independent packages. Database-backed suites
skip unless `HAUNT_TEST_DATABASE_URL` is explicitly set. The test helper verifies
the named isolated Docker container and loopback port before creating disposable
databases; never point it at a normal application or production database.

The current fixture expects `chatxo-identity-check` (PostgreSQL 16), published only
at `127.0.0.1:32768`. Its credentials must be configured for that disposable test
container. `internal/testutil/` owns the identity gate and database lifecycle.

Useful focused checks:

```sh
go test ./internal/config
go test ./internal/httpapi -run TestEmbeddedWebClient
go run ./cmd/apidocs -check -client-out client/static/api
python3 -m unittest discover -s scripts/tests
```

With the isolated database configured, the HTTP, identity and process suites test
real authorization, persistence, concurrency, media signaling and process recovery.
Race checks can be scoped to the changed packages using `go test -race`.

The client submodule provides unit tests, a production build and headless browser
integration harnesses. Its server fixtures are in `internal/clienttest/` so the
standalone client repository does not import server internals. Browser harnesses
use private profiles and own only the processes they launch.

The GitHub server workflow builds Linux amd64 and arm64 binaries with the pinned
web client embedded. Cross-compilation proves a binary can be produced, not that
all hardware, network or operating-system behavior works. Test the intended host,
TLS/proxy settings, microphone/camera devices and network paths before relying on
them. Large-room capacity and long-duration call performance need separate testing.
