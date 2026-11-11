package schwab

import (
	"context"
	"encoding/json"
)

// UserPreference is the user's preferences, from GET /userPreference: the
// accounts with their nicknames, and what the streamer needs. Schwab's spec
// says it is a list, but Schwab sends one object.
type UserPreference struct {
	Accounts []PreferenceAccount `json:"accounts"`
	// StreamerInfo and Offers have each held one element.
	StreamerInfo []StreamerInfo `json:"streamerInfo"`
	Offers       []Offer        `json:"offers"`

	// Problems and Raw are as the package doc describes.
	Problems []Problem       `json:"problems,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the preferences leniently, as the package doc
// describes. It never fails.
func (u *UserPreference) UnmarshalJSON(b []byte) error {
	type plain UserPreference
	var w struct {
		plain
		Problems json.RawMessage `json:"problems"` // Schwab's, as decode says
	}
	isObject, err := decode(b, &w)
	*u = UserPreference(w.plain)
	u.Problems, u.Raw = recordProblems(b, u, isObject, err, "problems"), copyRaw(b)
	return nil
}

// PreferenceAccount is an account as the user's preferences describe it.
type PreferenceAccount struct {
	AccountNumber  string `json:"accountNumber"`
	PrimaryAccount bool   `json:"primaryAccount"`
	// Type has been seen only as BROKERAGE, retirement accounts included.
	Type string `json:"type"`
	// NickName is the account's name on schwab.com, which the user can
	// change; it need not be unique.
	NickName string `json:"nickName"`
	// DisplayAcctID is the account number as Schwab masks it: "..." and its
	// last three digits.
	DisplayAcctID      string `json:"displayAcctId"`
	AutoPositionEffect bool   `json:"autoPositionEffect"`
	AccountColor       string `json:"accountColor"`
	// LotSelectionMethod, such as FIFO, has been seen in a capture of 2025
	// but not in those of 2024; it may depend on the account.
	LotSelectionMethod string `json:"lotSelectionMethod,omitzero"`
}

// StreamerInfo is how to reach Schwab's streamer. The customer and
// correlation IDs identify the user and the session; they are not
// credentials, but keep them out of logs.
type StreamerInfo struct {
	StreamerSocketURL      string `json:"streamerSocketUrl"`
	SchwabClientCustomerID string `json:"schwabClientCustomerId"`
	SchwabClientCorrelID   string `json:"schwabClientCorrelId"`
	SchwabClientChannel    string `json:"schwabClientChannel"`
	SchwabClientFunctionID string `json:"schwabClientFunctionId"`
}

// Offer is the user's market data permissions.
type Offer struct {
	Level2Permissions bool `json:"level2Permissions"`
	// MktDataPermission has been seen as NP.
	MktDataPermission string `json:"mktDataPermission"`
}

// UserPreference returns the user's preferences.
func (c *Client) UserPreference(ctx context.Context) (UserPreference, error) {
	r, err := c.get(ctx, "/trader/v1/userPreference", nil)
	if err != nil {
		return UserPreference{}, err
	}
	return one[UserPreference](r, "the user preferences")
}
