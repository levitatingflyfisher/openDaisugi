//! The floor on a terminal: the roster of every pane, grouped by task or
//! project, with live windows beside it on a wide screen, a peek on a
//! narrow one, a prompt line, the mouse, and a hand-off to attach for one
//! pane full screen.

pub mod facts;
pub mod firstrun;
pub mod keys;
pub mod model;
pub mod prompt;
pub mod rail;
pub mod railtree;
pub mod recent;
pub mod render;
pub mod run;
pub mod speak;
pub mod stackline;
pub mod tree;
pub mod voice;
pub mod voicekey;

#[cfg(test)]
mod tests;
#[cfg(test)]
mod tests_run;

pub use firstrun::first_run;
pub use run::{run, FloorErr, Options};
