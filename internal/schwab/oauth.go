package schwab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// NewState returns a random value for the state parameter of a login, which
// ties the redirect back to the login that started it.
func NewState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// AuthorizeURL returns the address where the browser login starts.
func (a *Auth) AuthorizeURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {a.cfg.AppKey},
		"redirect_uri":  {a.cfg.CallbackURL},
		"state":         {state},
	}
	return a.cfg.BaseURL + "/v1/oauth/authorize?" + q.Encode()
}

// CodeFromRedirect pulls the authorization code out of the address the
// browser was sent to after the login, as pasted by the user. A bare code, an
// address without its scheme and a bare query string are accepted too. The
// state in the address, if any, must be wantState.
//
// An error quotes nothing of what was pasted, which can hold the code, but
// the error and its description that Schwab put in the address, each cut to
// 200 bytes and quoted, so that a control character in them, such as one
// that moves the cursor, is shown rather than sent to the terminal.
func CodeFromRedirect(pasted, wantState string) (string, error) {
	pasted = strings.TrimSpace(pasted)
	if pasted == "" {
		return "", errors.New("nothing was pasted")
	}
	if !strings.Contains(pasted, "://") {
		if !strings.Contains(pasted, "code=") {
			if strings.Contains(pasted, "%") {
				if decoded, err := url.QueryUnescape(pasted); err == nil {
					return decoded, nil
				}
			}
			return pasted, nil
		}
		// An address copied without its scheme, or just its query string.
		pasted = "https://pasted/?" + pasted[strings.Index(pasted, "?")+1:]
	}
	u, err := url.Parse(pasted)
	if err != nil {
		// url.Parse's error quotes the address, or a part of it, which can
		// be the code.
		return "", errors.New("that doesn't look like a web address; paste the whole address from the browser's address bar")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		msg := fmt.Sprintf("Schwab reported %q", clip(e, 200))
		if d := q.Get("error_description"); d != "" {
			msg += fmt.Sprintf(": %q", clip(d, 200))
		}
		return "", errors.New(msg)
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("no code= in that address; paste the address the browser ended up on after you approved")
	}
	if got := q.Get("state"); got != "" && got != wantState {
		return "", errors.New("that address is from a different login attempt; start the login over")
	}
	return code, nil
}

// tokenResponse is the part of Schwab's token response that is kept.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// tokenRequest posts a grant to Schwab's token endpoint. The request runs to
// its end even if ctx is cancelled, since Schwab may grant it all the same,
// and a grant that is not saved can lose the login. It gets its own time
// limit instead, whatever the HTTP client's, so that a process holding the
// lock lets go of it in time.
func (a *Auth) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tokenRequestTimeout)
	defer cancel()
	var out tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.SetBasicAuth(a.cfg.AppKey, a.cfg.AppSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("%w: %w", errReading, err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, newError(resp.StatusCode, resp.Header, body)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("decoding token response: %w", err)
	}
	if out.AccessToken == "" {
		return out, errors.New("Schwab's token response had no access token")
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 1800
	}
	return out, nil
}
