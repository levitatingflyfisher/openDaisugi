//! `robot-probe`: a test instrument for clients/robotics_compare.py. It
//! answers the robotics cases with the MuJoCo executors, as
//! clients/robotics_cases.py answers them through the oracle. It is not
//! shipped, and only the mujoco feature builds it.

fn main() {
    std::process::exit(daisugi_verify::cli::robotprobe::main())
}
