package schwab

import "github.com/dterei/schwab-mcp/internal/nytime"

// newYorkZone is the time zone that nytime builds in, in the tz database's
// binary form, for the tests that load it or give it to a process as its
// zone.
var newYorkZone = nytime.TZData()
