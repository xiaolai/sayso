//go:build race

package canon

// Under the race detector TestEquivalentWorstCase reads texts raceFactor times shorter, and allows raceSlowdown times
// the time: a call that spends the whole work budget runs about six times slower instrumented.
const (
	raceFactor   = 8
	raceSlowdown = 8
)
