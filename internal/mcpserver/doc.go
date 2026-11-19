// Package mcpserver is schwab-mcp's MCP server: read-only tools, resources
// and a prompt over the user's Schwab accounts and the local history store,
// served through the MCP Go SDK to a client that runs it as a subprocess.
// cmd/schwab-mcp starts it; the client and the store know nothing of it.
//
// Each tool is registered through addTool, on the SDK's low-level
// Server.AddTool, so that the server decodes and checks the arguments, runs
// the handler, masks and encodes the answer once and checks it against the
// tool's output schema itself: the text block and the structured content
// carry the same bytes, compact, in the order the fields are declared, with
// HTML's characters as they are.
//
// # What must never happen
//
// Each of these is made impossible by how the code is built, as near as it
// gets, and a test catches a regression.
//
//   - A full account number reaches the model, a log line or an error. The
//     output types hold no account number: every string field has a kind
//     (kinds.go), and only the masker makes the kinds that hold text from
//     outside, FreeText, MaskedNumber and Path. Schwab's JSON goes through a
//     rewriter that masks by key and withholds what it does not know
//     (mask.JSON). Text the server composes, errors and warnings included,
//     is built from pieces already masked and never masked whole, which
//     would mangle dates and identifiers. Log lines pass a handler that
//     turns every value into text, masks error text as free text, and
//     replaces the known numbers, the secrets and the home folder. A last
//     pass, the backstop, replaces every account number the server has
//     seen, and the app key, secret and tokens, in every string that
//     leaves, structured content included, and tests fail on any hit.
//     TestOnlyTheMaskerMakesMaskedText, TestOutputStringFieldsHaveKinds and
//     TestNoAccountNumberLeaks hold this.
//   - The app secret or a token goes to another host than Schwab's. The
//     API's address comes from config.json alone, never the client's
//     environment, and schwab.NewAuth refuses plain http but to this
//     computer; auth_status names any host that is not Schwab's.
//   - The server dies. addTool, the receiving middleware and safeGo
//     recover a panic; nothing in the package calls panic or os.Exit but
//     registration at startup (TestPanicsAreErrors, TestServerConventions).
//   - The stdio stream breaks. cmd/schwab-mcp points file descriptor 1 at
//     stderr before serving, and the package never prints.
//   - A client fails to load the tool list. Every schema is held to a
//     portable subset with an object at its root (TestSchemasArePortable,
//     TestSchemasCompile, TestSchemaGoldens, TestToolNames).
//   - A call outlives the client's timeout. Each call has a budget, 45
//     seconds by default; no wait is begun that the budget would cut, and
//     a walk that runs short answers with what it has (TestDeadline).
//   - One large answer makes every call of its range fail for size. Pages
//     fill to a budget over the whole answer inside a hard cap, and an
//     answer over the cap on its own has its longest lists cut, each named
//     in cut, rather than being refused (TestSizeCap).
//   - A missing value shows as 0. Figures are lookup.Money, lookup.Qty,
//     lookup.Price and Num, whose zero values marshal as null.
//   - The server writes the store. It reads through history.OpenReader,
//     which has no writer, and the tests digest the store around the whole
//     test binary (TestServerNeverWritesTheStore).
//   - An account is guessed. The directory picks one account or none, and
//     names the others (TestAccountSelection).
//   - The server logs in. No code path calls Auth.Login or
//     Auth.AuthorizeURL, which lint refuses here; every login error asks the
//     user to run schwabtx login in their own terminal (TestLoginErrors).
package mcpserver
