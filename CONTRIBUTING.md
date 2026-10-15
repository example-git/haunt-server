# Contributing

This repository owns the Haunt server, database behavior, protocol contracts,
deployment examples, and container runtime. Changes to the embedded interface
may also require a compatible commit in the Haunt Client repository.

## Before submitting a change

- Keep installation data, credentials, private plugins, and deployment records
  outside the repository.
- Add tests for behavior and protocol changes.
- Update generated API contracts and documentation when their source changes.
- Update the `client/` submodule deliberately; do not track a moving branch.

Run the checks relevant to your change, including:

```sh
go test ./...
python3 scripts/tests/test_install.py
python3 scripts/check-repository.py
python3 scripts/check-docs.py
```

Container and embedded-client changes should also use the documented container
and client integration checks. Report vulnerabilities through the private
process in [SECURITY.md](SECURITY.md), not in a public issue.

