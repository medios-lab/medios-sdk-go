package main

// Partition is a contiguous slice [Lo, Hi] of the range 1..N with a stable
// index. The framework launches one task per partition; each task computes the
// sum of its own slice and writes it to stdout.
type Partition struct {
	Index int
	Lo    int64
	Hi    int64
}

// partition splits 1..n into m contiguous partitions whose sizes differ by at
// most one, covering 1..n exactly. Requires n >= 1 and m >= 1.
func partition(n int64, m int) []Partition {
	base := n / int64(m)
	rem := n % int64(m)
	parts := make([]Partition, 0, m)
	lo := int64(1)
	for i := 0; i < m; i++ {
		size := base
		if int64(i) < rem {
			size++
		}
		hi := lo + size - 1
		parts = append(parts, Partition{Index: i, Lo: lo, Hi: hi})
		lo = hi + 1
	}
	return parts
}

// totalSum returns 1 + 2 + ... + n, the answer the framework checks the
// aggregated task results against.
func totalSum(n int64) int64 { return n * (n + 1) / 2 }
