//go:build race

package history

// raceOn reports whether the race detector is on, under which SQLite runs
// 18 to 38 times slower: tests that repeat work do less of it then.
const raceOn = true
