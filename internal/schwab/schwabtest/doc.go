// Package schwabtest is a fake of Charles Schwab's Trader API for tests:
// its fixtures, which are answers in the shape Schwab sends them, and an
// index of them, which says what request each file answers, with what
// status and headers, and where it came from.
//
// It does not import package schwab, so that package's own tests can use
// it as well as everyone else's.
package schwabtest
