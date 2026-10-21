# Testing and platform coverage

The **Server regressions** workflow runs on main pushes and pull requests and is
required by the server release workflow. It uses the isolated PostgreSQL fixture,
race detection, real media dependencies, browser sign-in/DM/media/update checks,
standalone first-run setup and both Linux builds. Unexpected skipped Go tests fail
the job; the browser and first-run entry points run in separate explicit steps.
Evidence is retained as workflow artifacts for 14 days.

The manual **Call endurance** workflow accepts 30–1800 observation seconds per
transport, runs two browser peers sequentially over WebRTC and WebSocket, and
saves audible-playback and decoded-video samples. See the client's
[qualification guide](../client/docs/release-qualification.md) for its limits and
the physical-device/automatic-upgrade matrix. Independent encryption review has
a separate [review brief and source-packet command](security-review.md).

After deploying a release, compare its public hostname with the extracted
`web/` directory and the installation ID. Repeat `--origin` for configured aliases:

```sh
python3 scripts/check-deployment.py --origin https://chat.example.com \
  --web /path/to/release/web --instance EXISTING-INSTALLATION-ID \
  --output /path/to/deployment-check.json
```

This requires live health/readiness, the preserved installation identity, the
expected build marker and matching bytes for every release web asset. It does not
replace authenticated user-workflow or call testing.

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
