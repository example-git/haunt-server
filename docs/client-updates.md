# Desktop client distribution

Server operators can distribute the project's signed desktop builds to connected
clients. Set one source in the service environment:

```sh
# External manifest; this takes precedence over the local flags.
HAUNT_CLIENT_UPDATE_MANIFEST_URL=https://downloads.example.test/haunt-updates.json

# Alternatively, omit the URL and enable the private local mirror.
HAUNT_CLIENT_UPDATE_LOCAL=true
HAUNT_CLIENT_UPDATE_INSTALLERS=true
```

Both local flags default to false. `INSTALLERS` controls the web download button;
the updater can still obtain a full package when a runtime change requires it.
The instance response advertises the configured source. Local endpoints require
a signed-in account. Version/platform are reported at update checks; downloading
is bounded to four concurrent transfers with per-account rate limits. No GitHub
credentials reach clients and update files are separate from message storage.

Use `scripts/install-client-release.py --directory <data>/client-updates
--public-key /protected/update-public.pem` with a release tar.gz on stdin.
The importer verifies the administrator-installed release public key, file hashes
and sizes; it rejects path traversal, links and duplicate versions. It installs a
complete `client-vX.Y.Z/` directory before atomically replacing `haunt-updates.json`.
The service picks up new manifests without a restart. Environment changes require
a service restart. Retain previous version directories until existing downloads
have finished; delete older versions manually to control disk use.

The browser shows an installer download only for supported detected platforms with
a matching file in the local release. The desktop verifies releases against its
own pinned public key, even when the distribution server is untrusted. The service
cannot authorize a new signing key or force a running client to restart.

See the [client release guide](https://github.com/example-git/haunt-client/blob/main/docs/releases.md)
for signing, GitHub builds, ASAR rollback and full-package updates.
