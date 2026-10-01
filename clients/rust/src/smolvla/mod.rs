//! lerobot/smolvla_base from its three ONNX graphs: the tokenizer, the pre
//! and post processing and the Euler loop that stay outside the graphs
//! (ruling VL-R-3), and, with the mujoco feature, the graphs themselves
//! through ONNX Runtime.

#[cfg(feature = "mujoco")]
mod policy;
mod process;
mod tables_gen;
mod tokenizer;

#[cfg(feature = "mujoco")]
pub use policy::{Policy, CHUNK_TOLERANCE, PINS};
pub use process::{
    image, noise, step_time, time_embedding, Stats, ACTION_DIM, CHUNK, IMAGE_SIZE, STATE_DIM,
    STEPS, TIME_DIM,
};
pub use tokenizer::{Tokenizer, WIDTH};

#[cfg(all(test, feature = "mujoco"))]
mod policy_tests;
#[cfg(test)]
mod tests;
