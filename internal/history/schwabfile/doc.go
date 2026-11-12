// Package schwabfile reads the files that the store imports, taking their
// contents, never their paths: the History exports of schwab.com, in CSV
// (in every layout it has had) and in JSON; its Realized Gain/Loss
// exports' Lot Details; the snapshot files that `schwabtx snapshot`
// writes, whose lines it also writes; and what `schwabtx transactions
// -json` prints. Read tells a file by its content, never by its name or
// extension, and refuses Schwab's other files by what they are.
//
// It knows nothing of SQLite or of the store's tables, and keeps every
// value as the file writes it: numbers are Decimals, read exactly from
// their text, and free text is as written, for the store to mask. What a
// row cannot give, such as a date, is a problem of that row, which is kept;
// what a file as a whole shows, such as a stated total that its rows do not
// sum to, is a Warning.
package schwabfile
