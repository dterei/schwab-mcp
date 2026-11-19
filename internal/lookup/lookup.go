// Package lookup holds what schwabtx's lookups, accounts, balances and
// positions, work out from Schwab's answers, and what the MCP server works
// out the same way, so that the two never disagree on a figure: an
// account's value, cash and buying power, a position's quantity, cost and
// open profit or loss, which account a selector picks, which problems to
// name, and what an error from the login or the API means.
//
// A figure that Schwab did not send, or that could not be read, is none,
// never 0: Money, Qty and Price hold whether they have a value, print none
// as "-" and marshal it as null. Sums with a none in them are none.
package lookup
