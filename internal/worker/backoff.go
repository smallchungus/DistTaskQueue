package worker

import (
	"math"
	"math/rand/v2"
	"time"
)

func Compute(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	base := math.Pow(2, float64(attempts))
	if base > 600 {
		base = 600
	}
	jitter := 0.75 + rand.Float64()*0.5 //nolint:gosec // jitter spread, not security-sensitive
	return time.Duration(base * jitter * float64(time.Second))
}
