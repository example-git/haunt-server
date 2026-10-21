![Haunt](../../../assets/haunt-wordmark.png)

# Bundled RNNoise package

Source: https://github.com/example-git/haunt-audio-rnnoise

Plugin revision: `c32f984b4263e26675721c2ff68845873c570ee7`.
Upstream commit/model hashes, artifact hashes and compiler version: `build.json`.
Archive checksum: `SHA256SUMS`.

The ZIP intentionally ships with the server so a new installation can provide
noise suppression without downloading or running repository build scripts.
Update it only from a reviewed plugin revision and copy its ZIP, build metadata,
checksums and notices together. Keep the tests for seeding/removal and Wasm
validation passing. Never substitute a private operator package here.
