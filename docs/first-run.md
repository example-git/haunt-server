# First-time setup

The standalone server and all-in-one container use a one-time browser wizard
on their normal client/API listener. There is no additional setup port.

## Standalone binary

Run the binary with a persistent `HAUNT_DATA_DIR` (default `data`) and, if needed,
`HAUNT_LISTEN_ADDR` (default `127.0.0.1:8080`). Use a dedicated PostgreSQL database;
you can enter its connection in the wizard or provide `HAUNT_DATABASE_URL` through
the service environment. Haunt does not install a standalone database for you.

Open the private setup link printed at startup. The wizard walks through the
server name and public address, message/attachment limits and retention, call
capacity and networking, then the owner username and password. The public URL
must be HTTPS except for localhost and private-network IPs. For public setup,
put the normal service listener behind HTTPS and configure exact
`HAUNT_TRUSTED_PROXIES` CIDRs if the proxy terminates TLS.

After setup, the same listener serves the normal API and the embedded client.
Restarting reloads the selected configuration; it does not rerun the wizard or
replace the owner. Explicit environment values override saved settings and are
shown as locked fields during setup. Invalid explicit values fail visibly.

## All-in-one container

Follow the [container guide](container.md). The public hostname/certificate mode
and published ports are deployment settings supplied before startup, so HTTPS
can be available for the wizard. The database and installation ID are provisioned
inside the dedicated volume. Other settings and the owner credentials are
chosen in the wizard. No database password or generated owner password needs
to be extracted from the container. Open the private link from the startup log,
then use **Open Haunt** when configuration finishes.

## Ownership and persistence

A random 256-bit installer code authorizes first-owner creation. It is delivered
in the startup link's fragment, removed from browser history, and sent only in
the setup request's authorization header. Setup requires same-origin submission
and HTTPS for public addresses; forwarding headers are accepted only from
configured trusted peers. Do not share the installer link or service logs while
setup is pending. The code survives an interrupted setup in a private file and
is removed once the owner has been created.

The configuration file is `HAUNT_DATA_DIR/configuration.json`, written atomically
with mode `0600`. A standalone database connection may contain credentials in
that private file; protect it as part of your installation backup. Deployment
secrets supplied through the environment are not copied there. Owner passwords
are never saved in the configuration or startup logs; PostgreSQL stores an
Argon2id password hash. Preserve the data directory and database together.

Existing installations skip setup when their original instance has an owner.
Setup refuses a database with an existing owner and cannot reset their password.
Preserve the installation ID during migrations. Explicit bootstrap credentials
remain supported for unattended fresh installations. Owner password recovery
still uses the separate host-local procedure in [operations](operations.md).

While waiting for setup, `/readyz` reports `status: setup`; after submission it
reports unavailable until normal service initialization finishes. Only the normal
service then reports `status: ok`. Container health can indicate that the setup
page is reachable without claiming that a community is configured.
