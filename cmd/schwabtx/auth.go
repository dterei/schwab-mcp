package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Schwab refresh tokens stop working 7 days after the browser login that
// issued them; refreshing the access token does not extend that.
const refreshTokenLifetime = 7 * 24 * time.Hour

type token struct {
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token"`
	AccessExpiresAt   time.Time `json:"access_expires_at"`
	RefreshObtainedAt time.Time `json:"refresh_obtained_at"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type authenticator struct {
	cfg  config
	http *http.Client
	tok  *token
}

func newAuthenticator(cfg config, httpClient *http.Client) *authenticator {
	return &authenticator{cfg: cfg, http: httpClient}
}

func (a *authenticator) authorizeURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {a.cfg.AppKey},
		"redirect_uri":  {a.cfg.CallbackURL},
		"state":         {state},
	}
	return a.cfg.BaseURL + "/v1/oauth/authorize?" + q.Encode()
}

// load reads the saved token from disk.
func (a *authenticator) load() error {
	b, err := os.ReadFile(a.cfg.TokenFile)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("not logged in yet; run `schwabtx login` first")
	}
	if err != nil {
		return fmt.Errorf("reading token file: %w", err)
	}
	var t token
	if err := json.Unmarshal(b, &t); err != nil {
		return fmt.Errorf("token file %s is corrupt (%w); run `schwabtx login` again", a.cfg.TokenFile, err)
	}
	if t.RefreshToken == "" {
		return fmt.Errorf("token file %s has no refresh token; run `schwabtx login` again", a.cfg.TokenFile)
	}
	a.tok = &t
	return nil
}

func (a *authenticator) save() error {
	b, err := json.MarshalIndent(a.tok, "", "  ") //nolint:gosec // G117: holding the tokens is this file's job.
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.cfg.TokenFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating token directory: %w", err)
	}
	// Write to a temp file and rename so a crash never leaves half a token.
	tmp, err := os.CreateTemp(dir, ".token-*.json")
	if err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("writing token file: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("writing token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	if err := os.Rename(tmp.Name(), a.cfg.TokenFile); err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	return nil
}

// expiryWarning returns a message when the weekly re-login is less than a day away.
func (a *authenticator) expiryWarning() string {
	if a.tok == nil || a.tok.RefreshObtainedAt.IsZero() {
		return ""
	}
	left := a.tok.RefreshObtainedAt.Add(refreshTokenLifetime).Sub(timeNow())
	if left <= 0 || left > 24*time.Hour {
		return ""
	}
	when := "in less than an hour"
	if hours := int(left.Round(time.Hour) / time.Hour); hours == 1 {
		when = "in about an hour"
	} else if hours > 1 {
		when = fmt.Sprintf("in about %d hours", hours)
	}
	return fmt.Sprintf("your Schwab login expires %s; run `schwabtx login` to renew it", when)
}

// accessToken returns a usable access token, refreshing it first if it has
// expired or is about to.
func (a *authenticator) accessToken(ctx context.Context) (string, error) {
	if a.tok == nil {
		return "", errors.New("not logged in yet; run `schwabtx login` first")
	}
	if a.tok.AccessToken != "" && timeNow().Before(a.tok.AccessExpiresAt.Add(-time.Minute)) {
		return a.tok.AccessToken, nil
	}
	if err := a.refresh(ctx); err != nil {
		return "", err
	}
	return a.tok.AccessToken, nil
}

func (a *authenticator) refresh(ctx context.Context) error {
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.tok.RefreshToken},
	})
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return fmt.Errorf("Schwab rejected the app key or secret; check SCHWAB_APP_KEY and SCHWAB_APP_SECRET and that the app is \"Ready For Use\": %w", err)
		}
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
			return fmt.Errorf("Schwab rejected the saved login (it expires 7 days after you log in); run `schwabtx login` again: %w", err)
		}
		return fmt.Errorf("refreshing access token: %w", err)
	}
	a.tok.AccessToken = res.AccessToken
	a.tok.AccessExpiresAt = timeNow().Add(time.Duration(res.ExpiresIn) * time.Second)
	if res.RefreshToken != "" {
		a.tok.RefreshToken = res.RefreshToken
	}
	return a.save()
}

func (a *authenticator) exchangeCode(ctx context.Context, code string) error {
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {a.cfg.CallbackURL},
	})
	if err != nil {
		return fmt.Errorf("exchanging the authorization code: %w", err)
	}
	if res.RefreshToken == "" {
		return errors.New("Schwab's token response had no refresh token")
	}
	now := timeNow()
	a.tok = &token{
		AccessToken:       res.AccessToken,
		RefreshToken:      res.RefreshToken,
		AccessExpiresAt:   now.Add(time.Duration(res.ExpiresIn) * time.Second),
		RefreshObtainedAt: now,
	}
	return a.save()
}

func (a *authenticator) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	var out tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.SetBasicAuth(a.cfg.AppKey, a.cfg.AppSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, &apiError{Status: resp.StatusCode, Body: string(body)}
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

// login walks through the browser authorization. Schwab only allows https
// callback URLs, so rather than run a local TLS server we have the user paste
// back the address their browser was redirected to.
func login(ctx context.Context, a *authenticator, stdin io.Reader, out io.Writer) error {
	state, err := randomState()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, `1. Open this link in your browser and sign in with your Schwab brokerage login:

   %s

2. Tick the accounts you want to share and approve.
3. Your browser then lands on %s/... and shows a "can't connect" error.
   That is expected. Copy the whole address from the address bar.
4. Paste it here and press Enter (be quick, the code in it expires in about 30 seconds):

> `, a.authorizeURL(state), a.cfg.CallbackURL)

	// Read in the background so Ctrl-C still works while we wait for the paste.
	type pasted struct {
		line string
		err  error
	}
	ch := make(chan pasted, 1)
	go func() {
		line, err := bufio.NewReaderSize(stdin, 64*1024).ReadString('\n')
		ch <- pasted{line, err}
	}()
	var p pasted
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p = <-ch:
	}
	if p.err != nil && (!errors.Is(p.err, io.EOF) || strings.TrimSpace(p.line) == "") {
		return fmt.Errorf("reading the pasted address: %w", p.err)
	}
	code, err := extractCode(p.line, state)
	if err != nil {
		return err
	}
	if err := a.exchangeCode(ctx, code); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nLogged in. Token saved to %s.\nSchwab expires this login after 7 days, so run `schwabtx login` again when it does.\n", a.cfg.TokenFile)
	return nil
}

// extractCode pulls the authorization code out of the pasted redirect address.
// A bare code is accepted too.
func extractCode(pasted, wantState string) (string, error) {
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
		return "", fmt.Errorf("that doesn't look like a web address: %w", err)
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("Schwab reported %q: %s", e, q.Get("error_description"))
	}
	code := q.Get("code")
	if code == "" {
		return "", errors.New("no code= in that address; paste the address the browser ended up on after you approved")
	}
	if got := q.Get("state"); got != "" && got != wantState {
		return "", errors.New("that address is from a different login attempt; start `schwabtx login` over")
	}
	return code, nil
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
