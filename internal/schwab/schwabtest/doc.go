// Package schwabtest is a fake of Charles Schwab's Trader API for tests:
// its fixtures, which are answers in the shape Schwab sends them.
//
// It does not import package schwab, so that package's own tests can use
// it as well as everyone else's.
package schwabtest
