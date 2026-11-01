//! The token-saving router: the HTTP proxy between a harness and its
//! model, the rules router in front of it, the meter and the turn journal
//! behind it. It is `opendaisugi.router`, `router_asgi`,
//! `router_pipeline`, `router_journal`, `router_answers`,
//! `router_openai`, `router_report` and `routing`, with the same bytes on
//! the wire and in the files. The Go client's `router` package is the
//! reference.

pub mod answers;
pub mod http;
pub mod knn;
pub mod model;
pub mod meter;
pub mod pipeline;
pub mod pyops;
pub mod route;
pub mod server;
pub mod sniff;
pub mod report;
pub mod recall;
