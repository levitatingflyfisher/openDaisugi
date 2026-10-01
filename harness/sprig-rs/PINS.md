# sprig-rs pins

What this crate builds against, and why each one is pinned. The crate is
`sprig-rs`; the binaries it builds are `sprig`, `sprig-hook`, `sprig-mcp`
and `weave`, as the Go module's are.

| Pin | Value | Why |
|---|---|---|
| Rust | 1.98.0 | The version CI installs (`RUST_VERSION` in `.github/workflows/clients.yml`). Builds run `--locked`. |
| rustls | =0.23.45, `default-features = false`, features `ring`, `std`, `tls12` | https for the API backend, as Go's net/http gives it. Apache-2.0 OR ISC OR MIT, the rustls project, 2.0 MB of source. The version and features `harness/coppice-rs` and `clients/rust` lock (SP-R-4). |
| ring | =0.17.14 | rustls's crypto. Apache-2.0 AND ISC, Brian Smith's ring (from BoringSSL), 8.2 MB of source, most of it pregenerated assembly; its C is built with the system compiler through the `cc` crate. Already in `harness/coppice-rs` and `clients/rust`. |
| rustls-pki-types | =1.15.1, feature `std` | The certificate types rustls takes. MIT OR Apache-2.0, the rustls project. Already in `harness/coppice-rs` and `clients/rust`. |

Every other crate in `Cargo.lock` comes in through these, at the versions
`harness/coppice-rs/Cargo.lock` holds. `NOTICE` lists every crate in the
binary and its licence.

There is no JSON crate. sprig reads and writes JSON as Go's encoding/json
does, and serde_json does not: it refuses a lone `\ud800` and bytes that
are not UTF-8, which Go turns into U+FFFD, and it does not match keys
case-folded, reuse slice elements or give Go's error words. `src/json.rs`
does these by hand.

The system roots for https are read as Go's crypto/x509 reads them on
Linux: `SSL_CERT_FILE`, else the first bundle found in the usual places.

Build:

```
cargo build --release --locked
```

`SPRIG_VERSION` sets what `sprig --version` prints; without it, the build
stamps the checkout's commit, as the Go build does.
