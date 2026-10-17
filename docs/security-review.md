# Independent encryption review brief

Status: **prepared for review; no independent review has been completed**.
Assign an external reviewer before changing that status. A passing test suite or
an automated source review does not establish cryptographic security.

## Reproducible scope

After committing and pinning the client, run:

```sh
python3 scripts/package-security-review.py var/security-review/REVIEW-ID
```

The packet includes only Git-tracked source, exact server/client revisions,
archive hashes and this brief. It excludes working installation data, credentials,
private plugins and session caches. The manifest requires the server's client pin
to match the client snapshot. Use disposable test accounts; do not give reviewers
live credentials or user message archives.

Start with [the DM trust model](dm-security.md), then trace:

- `client/native/dm-crypto/`: OpenMLS engine, credential validation, group state,
  key packages, persistence and old/new state compatibility.
- `client/packages/browser/src/dm-crypto/`: device enrollment, worker boundaries,
  IndexedDB transactions, Web Locks, membership changes and recovery.
- `client/packages/core/src/backup.ts` and
  `client/packages/browser/src/dm-crypto/backup.ts`: authenticated backup formats,
  bounds, restored read state and fresh sending identities.
- `internal/chat/mls.go` and `internal/httpapi/dm.go`: device-directory trust,
  conversation authorization, compare-and-swap commits, replay and retention.
- `client/packages/browser/src/sync/`: optional secret derivation, OPAQUE/client
  binding, encrypted persistence and credential-change recovery. Include a
  separately pinned Haunt Sync source packet if reviewing that service too.

## Questions requiring independent findings

1. Can concurrent sends, persistence failures, retries, restarts or restored backups
   reuse an MLS encryption nonce or publish ciphertext before durable ratchet state?
2. Can membership conflicts, key-package replay, revoked devices or forged directory
   responses grant unexpected access? Preserve the documented limitation: the
   service is trusted for device identity and there is no key transparency.
3. Are encrypted images, keys and authenticated data bound to the correct sender,
   upload and conversation? Are malformed payloads and resource limits enforced?
4. Can imports, legacy plaintext records or unavailable keys downgrade the UI's
   security indication or silently activate plaintext sending?
5. Do backup, caches, exports, crash reports and sync expose secrets outside their
   documented boundaries? Can secret changes report success while leaving old
   authentication or encryption paths active?
6. Can web delivery, update verification, worker messages or dependencies bypass
   those guarantees? Separate compromised-client/server assumptions from flaws
   within the claimed trust model.

## Evidence and acceptance

Attach regression run URLs, secure-DM browser results, backup/restore results,
dependency lockfiles and reproduction commands. Tests support the review but do
not establish safety. The report must identify reviewer and independence, dates,
revisions, scope, methods, findings, reproductions, exclusions and limitations.
Track remediation with a revision and reviewer retest; retain unresolved findings.
Acceptance requires an actual report and resolution of release-blocking findings
agreed with the reviewer. This brief does not complete the review. Use
[SECURITY.md](../SECURITY.md) for private disclosure.
