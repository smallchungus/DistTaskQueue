package worker

import (
	"testing"
	"time"
)

func TestCompute_GrowsExponentially(t *testing.T) {
	cases := []struct {
		attempts int
		minD     time.Duration
		maxD     time.Duration
	}{
		{1, 1500 * time.Millisecond, 2500 * time.Millisecond},
		{2, 3 * time.Second, 5 * time.Second},
		{3, 6 * time.Second, 10 * time.Second},
		{4, 12 * time.Second, 20 * time.Second},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			d := Compute(c.attempts)
			if d < c.minD || d > c.maxD {
				t.Fatalf("attempts=%d got %v, want in [%v,%v]", c.attempts, d, c.minD, c.maxD)
			}
		})
	}
}

func TestCompute_CapsAt600s(t *testing.T) {
	d := Compute(20)
	if d < 450*time.Second || d > 750*time.Second {
		t.Fatalf("got %v, want ~600s ±25%%", d)
	}
}
