//! The token-saving gateway: the HTTP proxy between a harness and its
//! model, the rules router in front of it, the meter and the turn journal
//! behind it. It is `opendaisugi.gateway`, `gateway_asgi`,
//! `gateway_pipeline`, `gateway_journal`, `gateway_answers`,
//! `gateway_openai`, `gateway_report` and `routing`, with the same bytes on
//! the wire and in the files. The Go client's `gateway` package is the
//! reference.

pub mod answers;
pub mod http;
pub mod meter;
pub mod pipeline;
pub mod pyops;
pub mod route;
pub mod server;
pub mod sniff;
pub mod report;
pub mod recall;
