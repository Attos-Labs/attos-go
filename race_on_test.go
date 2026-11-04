//go:build race

package attos_test

// sync.Pool deliberately drops items under -race, so allocation counts are not meaningful.
const raceEnabled = true
