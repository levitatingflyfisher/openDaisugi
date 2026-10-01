# coppice-rs pins

What this crate builds against, and why each one is pinned. The crate is
`coppice-rs`; the binary it builds is `coppice`, as the Go one is.

| Pin | Value | Why |
|---|---|---|
| Rust | 1.98.0 | The version CI installs (`RUST_VERSION` in `.github/workflows/clients.yml`). Builds run `--offline --locked`. |
| libghostty-vt | ghostty `b0c421fcd2e290629d4285c181b52fe2f2095f06`, zig 0.16.0 | The commit the native prefix builds (`clients/go/scripts/native.sh`) and the Go coppice's `scripts/toolchain.sh` builds. One engine on both sides, so a screen reads the same in Go and Rust. `build.rs` takes the prefix from `COPPICE_GHOSTTY_PREFIX` and links `lib/libghostty-vt.a` statically. |
| The C layer | `csrc/vt_shim.c` | Built by the system C compiler (`CC`, else `cc`) and `ar`, against the prefix's own headers, so no Rust code declares a ghostty struct. It makes the calls go-libghostty makes for coppice's `internal/vt`. |
| serde, serde_json | =1.0.229, =1.0.151 | JSON in and out. `preserve_order` keeps a struct's field order, `raw_value` keeps a request's raw parameters as Go's `json.RawMessage` does, `arbitrary_precision` keeps a number's text. MIT or Apache-2.0, the serde-rs project. Already in `clients/rust`. |
| libc | =0.2.189 | The pty, SO_PEERCRED, flock, signals. MIT or Apache-2.0, the rust-lang project. Already in `clients/rust`. |
| toml | =0.8.23 (parse only) | `coppice.toml` and the agent-detection manifests. 0.8 reads TOML 1.0, as BurntSushi/toml 1.5 in the Go coppice does; toml 1.x reads TOML 1.1. MIT or Apache-2.0, the toml-rs project. |
| regex | =1.13.1, features `std` and `unicode` only | The manifests' `regex` and `line_regex` patterns. Herdr wrote them for this crate; the Go coppice reads them with Go's regexp after a rewrite, and `src/detect.rs` rewrites them to Go's meaning (CP-R-15). It brings regex-automata 0.4.18 and regex-syntax 0.8.11, the versions `clients/rust` locks, about 1 MB of source. MIT or Apache-2.0, the rust-lang project. |
| rustls | =0.23.45, `default-features = false`, features `ring`, `std`, `tls12` | TLS for `coppice web serve`, and the client for an https ntfy or voice server. Apache-2.0 OR ISC OR MIT, the rustls project, 2.0 MB of source. The version and features clients/rust locks (CP-R-36). |
| ring | =0.17.14 | P-256 keys, ECDSA signatures and SHA-256 for the local CA, and rustls's crypto. Apache-2.0 AND ISC, Brian Smith's ring (from BoringSSL), 8.2 MB of source, most of it pregenerated assembly; its C is built with the system compiler through the `cc` crate. Already in clients/rust. |
| rustls-pki-types | =1.15.1, feature `std` | The key and certificate types rustls takes. MIT OR Apache-2.0, the rustls project. Already in clients/rust. |
| The phone page | `harness/coppice/internal/web/static` | Embedded by `build.rs` as Go's `//go:embed static` embeds it: every file, with every name that starts with `.` or `_` left out at any depth. One copy of the page for both binaries; a unit test holds the list and the bytes to the Go tree. |
| The manifests and the foreman page | `harness/coppice/internal/detect/manifests/*.toml`, `harness/coppice/skills/foreman/SKILL.md` | Compiled in with `include_str!` from the Go tree, so both ports carry the same bytes. |

Every other crate in `Cargo.lock` comes in through these. aho-corasick
is there as regex's optional dependency; with `perf` off it is not
built. memchr comes in through serde_json. `NOTICE` lists every crate in
the binary and its licence. Nothing else is
fetched: a new crate must earn its place by license, provenance and size,
and be added here with its reason.

Build, with the prefix the repo's native script names:

```
export COPPICE_GHOSTTY_PREFIX=$(../../clients/go/scripts/native.sh --print-prefix)
cargo build --release --offline --locked
```

Unix only (Linux): SO_PEERCRED and `/proc` place a socket peer.
