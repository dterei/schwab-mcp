# schwabtx

A small Go CLI that logs in to the Charles Schwab Trader API and prints your
most recent transactions across every account you share with it. No
dependencies outside the Go standard library.

## Why the Schwab Trader API

| Option | Verdict |
| --- | --- |
| **Schwab Trader API** (developer.schwab.com, "Individual Developer") | **Picked.** Free, first-party, nobody else holds your data, and it returns the most detail per transaction (each fee, the security leg, order ids). |
| Plaid | Works for personal use and would also cover Schwab Bank checking and other institutions, but it puts a third party in the middle, flattens the data, refreshes overnight, and needs a web page (Plaid Link) just to log in. Worth revisiting if you want non-Schwab accounts. |
| SnapTrade and other aggregators | Same trade-offs as Plaid. |
| CSV export from schwab.com | Zero setup and reaches further back, but manual. A good way to backfill history later. |

Limits to know about:

- It covers **brokerage accounts**, not Schwab Bank checking or savings.
- Schwab makes you log in through the browser again **every 7 days**.
- History is served in limited date windows, so the tool walks backwards 60
  days at a time (`-window`) up to one year (`-lookback`). If Schwab refuses an
  older window the tool prints what it got and warns you.

## One-time setup (needs you)

You need a Schwab self-directed brokerage account; the Individual tier is free
and only for accessing your own accounts. These steps come from community
guides, so menu names on the portal may differ slightly.

1. Go to <https://developer.schwab.com>, click Register and create a developer
   account (a separate login from your brokerage one; using the same email is
   the safe choice). Pick the **Individual Developer** profile.
2. On the API Products page open **Trader API - Individual** and click Request
   Access if it asks.
3. Dashboard > Apps > Create App. Add the **Accounts and Trading Production**
   API product. Set the callback URL to `https://127.0.0.1`.
4. Wait for the app status to change from "Approved - Pending" to
   **"Ready For Use"**. Usually a few days, sometimes longer; login fails until
   then.
5. Copy the App Key and Secret into `.env` (see `.env.example`). Never commit
   that file or paste the values anywhere.

Some guides say the brokerage account must be thinkorswim-enabled for API calls
to work. If login succeeds but calls fail, enable thinkorswim on schwab.com.

## Run

```sh
set -a; source .env; set +a

go run . login      # opens nothing itself: follow the printed steps
go run .            # the 100 most recent transactions, all accounts
```

`login` prints a Schwab link. Sign in, tick the accounts to share, approve,
and your browser lands on `https://127.0.0.1/?code=...` with a connection
error. That is expected. Paste that whole address back into the terminal
within about 30 seconds.

Flags:

```
-n 100             how many transactions to print
-lookback 365      how many days back to search
-window 60         days per API request
-types TRADE,...   only these transaction types (default: all)
-json              Schwab's raw JSON instead of a table (best input for calculators;
                   includes full account numbers)
-show-accounts     full account numbers in the table instead of the last 4 digits
```

The login token is stored in your user config directory under
`schwabtx/token.json` with owner-only permissions. Override with
`SCHWAB_TOKEN_FILE`.

## Tests

```sh
go test ./...
```

The tests run the whole tool against a fake Schwab server: login, token
refresh, paging backwards through windows, merging accounts, splitting
truncated responses and rate-limit retries.

## What has not been verified

The code has never talked to the real Schwab API: that needs your app key and
a browser login. Endpoint paths, parameters and the response shape follow
Schwab's published Trader API as documented by community clients. Things most
likely to need a tweak on first real run:

- how far back Schwab actually serves transactions (sources disagree between
  60 days and one year), which only affects `-lookback` and `-window`;
- an empty list usually means the account was not ticked during login.

## Troubleshooting

- `HTTP 401` at login: the app is not "Ready For Use" yet, or the key/secret
  is wrong.
- `invalid_grant` / `HTTP 400` at login: the pasted code expired (be quicker)
  or `SCHWAB_CALLBACK_URL` does not match the app's callback URL exactly.
- "Schwab rejected the saved login": the 7 days are up, run `login` again.
