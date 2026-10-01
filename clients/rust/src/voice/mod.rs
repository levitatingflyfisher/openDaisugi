//! The voice bridge (`opendaisugi.voice`, stage H): record anywhere,
//! transcribe on this box with a resident engine or whisper.cpp, land the text in a pane through
//! coppice.

pub mod arm;
pub mod audio;
pub mod cleanup;
pub mod coppice;
pub mod engine;
pub mod hardware;
pub mod moonshine;
pub mod parakeet;
pub mod probe;
pub mod ptt;
pub mod pynum;
pub mod resident;
pub mod server;
pub mod wav;
