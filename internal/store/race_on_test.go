//go:build race

package store

// raceEnabled lets heavy tests shrink under the race detector.
const raceEnabled = true
