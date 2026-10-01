//go:build mujoco

package robotics

import "math"

// The damped least-squares update of the oracle's IK, computed in the
// order numpy computes it, so the ports reach the same joint targets bit
// for bit:
//
//	JJt = jacp @ jacp.T + damping2 * eye(3)
//	dq  = jacp.T @ solve(JJt, err)
//
// numpy hands the product to OpenBLAS, whose kernels on a CPU with FMA
// sum each entry as a chain of fused multiply-adds from zero; solve is
// LAPACK's gesv, which OpenBLAS factors with a left-looking LU (getf2:
// dot products, then the pivot, then a scale by the reciprocal) and
// solves with column-oriented triangular sweeps; jacp.T @ x is a plain
// sum. Each product below is rounded on its own (float64(a*b)) so the Go
// compiler cannot fuse it where numpy does not.
func dampedStep(jacp []float64, e [3]float64, damping2 float64) []float64 {
	nv := len(jacp) / 3
	var a [3][3]float64
	for i := 0; i < 3; i++ {
		for k := 0; k < 3; k++ {
			s := 0.0
			for j := 0; j < nv; j++ {
				s = math.FMA(jacp[i*nv+j], jacp[k*nv+j], s)
			}
			d := 0.0
			if i == k {
				d = damping2
			}
			a[i][k] = s + d
		}
	}
	x := solve3(a, e)
	dq := make([]float64, nv)
	for j := 0; j < nv; j++ {
		s := 0.0
		for i := 0; i < 3; i++ {
			s += float64(jacp[i*nv+j] * x[i])
		}
		dq[j] = s
	}
	return dq
}

// solve3 is OpenBLAS's dgesv on a 3 x 3 system.
func solve3(a [3][3]float64, b [3]float64) [3]float64 {
	const n = 3
	for j := 0; j < n; j++ {
		for i := 1; i < j; i++ {
			s := 0.0
			for k := 0; k < i; k++ {
				s += float64(a[i][k] * a[k][j])
			}
			a[i][j] -= s
		}
		for i := j; i < n; i++ {
			s := 0.0
			for k := 0; k < j; k++ {
				s += float64(a[i][k] * a[k][j])
			}
			a[i][j] -= s
		}
		p := j
		for i := j + 1; i < n; i++ {
			if math.Abs(a[i][j]) > math.Abs(a[p][j]) {
				p = i
			}
		}
		if p != j {
			a[j], a[p] = a[p], a[j]
			b[j], b[p] = b[p], b[j]
		}
		r := 1.0 / a[j][j]
		for i := j + 1; i < n; i++ {
			a[i][j] *= r
		}
	}
	for k := 0; k < n; k++ {
		for i := k + 1; i < n; i++ {
			b[i] -= float64(b[k] * a[i][k])
		}
	}
	for k := n - 1; k >= 0; k-- {
		b[k] /= a[k][k]
		for i := 0; i < k; i++ {
			b[i] -= float64(b[k] * a[i][k])
		}
	}
	return b
}

// norm3 is numpy.linalg.norm of a 3-vector: sqrt(x . x), summed in order.
func norm3(e [3]float64) float64 {
	s := 0.0
	for _, v := range e {
		s += float64(v * v)
	}
	return math.Sqrt(s)
}
