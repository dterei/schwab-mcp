package schwabtest

// A Route is an endpoint of the API that the fake serves, as the pattern of
// a ServeMux: a method and a path. A request's Pattern is its Route.
type Route string

// The Routes: Schwab's account endpoints, and one of the fake's own for
// tests of the client that need a path that answers.
const (
	AccountNumbersRoute Route = "GET /trader/v1/accounts/accountNumbers"
	AccountsRoute       Route = "GET /trader/v1/accounts"
	AccountRoute        Route = "GET /trader/v1/accounts/{hash}"
	TransactionsRoute   Route = "GET /trader/v1/accounts/{hash}/transactions"
	TransactionRoute    Route = "GET /trader/v1/accounts/{hash}/transactions/{id}"
	UserPreferenceRoute Route = "GET /trader/v1/userPreference"
	// TestRoute answers {"ok":true}. It is not Schwab's.
	TestRoute Route = "GET /trader/v1/test"
)

// allRoutes are the Routes, in the order above.
var allRoutes = []Route{
	AccountNumbersRoute, AccountsRoute, AccountRoute, TransactionsRoute, TransactionRoute,
	UserPreferenceRoute, TestRoute,
}
