//! The damped least-squares update of the oracle's IK, computed in the
//! order numpy computes it, so the ports reach the same joint targets bit
//! for bit:
//!
//! ```text
//! JJt = jacp @ jacp.T + damping2 * eye(3)
//! dq  = jacp.T @ solve(JJt, err)
//! ```
//!
//! numpy hands the product to OpenBLAS, whose kernels on a CPU with FMA
//! sum each entry as a chain of fused multiply-adds from zero; solve is
//! LAPACK's gesv, which OpenBLAS factors with a left-looking LU (getf2:
//! dot products, then the pivot, then a scale by the reciprocal) and
//! solves with column-oriented triangular sweeps; jacp.T @ x is a plain
//! sum. Rust never fuses a product on its own, so only the mul_add calls
//! fuse.

/// The update dq for the 3 x nv Jacobian jacp (row major) and the error e.
pub fn damped_step(jacp: &[f64], e: &[f64; 3], damping2: f64) -> Vec<f64> {
    let nv = jacp.len() / 3;
    let mut a = [[0.0f64; 3]; 3];
    for i in 0..3 {
        for k in 0..3 {
            let mut s = 0.0f64;
            for j in 0..nv {
                s = jacp[i * nv + j].mul_add(jacp[k * nv + j], s);
            }
            let d = if i == k { damping2 } else { 0.0 };
            a[i][k] = s + d;
        }
    }
    let x = solve3(a, *e);
    (0..nv)
        .map(|j| {
            let mut s = 0.0f64;
            for i in 0..3 {
                s += jacp[i * nv + j] * x[i];
            }
            s
        })
        .collect()
}

/// OpenBLAS's dgesv on a 3 x 3 system.
pub fn solve3(mut a: [[f64; 3]; 3], mut b: [f64; 3]) -> [f64; 3] {
    const N: usize = 3;
    for j in 0..N {
        for i in 1..j {
            let mut s = 0.0f64;
            for k in 0..i {
                s += a[i][k] * a[k][j];
            }
            a[i][j] -= s;
        }
        for i in j..N {
            let mut s = 0.0f64;
            for k in 0..j {
                s += a[i][k] * a[k][j];
            }
            a[i][j] -= s;
        }
        let mut p = j;
        for i in j + 1..N {
            if a[i][j].abs() > a[p][j].abs() {
                p = i;
            }
        }
        if p != j {
            a.swap(j, p);
            b.swap(j, p);
        }
        let r = 1.0 / a[j][j];
        for row in a.iter_mut().skip(j + 1) {
            row[j] *= r;
        }
    }
    for k in 0..N {
        for i in k + 1..N {
            b[i] -= b[k] * a[i][k];
        }
    }
    for k in (0..N).rev() {
        b[k] /= a[k][k];
        for i in 0..k {
            b[i] -= b[k] * a[i][k];
        }
    }
    b
}

/// numpy.linalg.norm of a 3-vector: sqrt(x . x), summed in order.
pub fn norm3(e: &[f64; 3]) -> f64 {
    let mut s = 0.0f64;
    for v in e {
        s += v * v;
    }
    s.sqrt()
}
