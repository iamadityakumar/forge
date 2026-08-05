package metrics

// Standard histogram bucket definitions shared across the system.
var (
	LatencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	StepBuckets    = []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
)
