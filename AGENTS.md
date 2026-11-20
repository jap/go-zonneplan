# AGENTS.md

Guidance for AI coding agents working in this repository.

## Project Overview

- `go-zonneplan` is a generic Go client module for the Zonneplan API (app-api.zonneplan.nl).
- Module path: `github.com/jap/go-zonneplan`.
- Primary consumer is evcc (../evcc), integrated LOCAL first (not an upstream PR): the wrapper lives in evcc's `tariff/` package. This module must stay independent of evcc (no evcc imports).
- Scope for now: rates/tariffs only (auth + consumer price charts). Other endpoints (battery, charge point, consumption) are out of scope until requested.
- Reference implementation: ../pyzonneplan (Python). When in doubt about API behavior, check there first.

## Essential Commands

- `go build ./...` - compile
- `go test ./...` - run all tests
- `go vet ./...` and `golangci-lint run` - lint
- `go test -race ./...` - run tests with race detector

## Coding Style

Follow the evcc Go conventions (see ../evcc/AGENTS.md). The essentials:

- Idiomatic Go: gofmt, self-documenting names, early returns.
- `context.Context` as first parameter for all I/O.
- Wrap errors with context: `fmt.Errorf("zonneplan: ...: %w", err)`. Use `errors.Is`/`errors.As` for checking. Exported sentinel error variables (e.g. `ErrNotFound`, `ErrRateLimit`) rather than string matching.
- Comments: comment the why, not the what. Exported identifiers get godoc comments starting with the identifier name. No em dashes.
- Validate interface implementations: `var _ api.Interface = (*Type)(nil)` where applicable (not needed until we define interfaces).
- Tests: `testing` with `testify/assert` and `testify/require`, table-driven where multiple cases exist. HTTP fixtures via `net/http/httptest`; fixture JSON lives in `testdata/`, copied from ../pyzonneplan/tests/fixtures so both clients test against identical payloads.
- Keep dependencies minimal: stdlib `net/http` + `encoding/json` only. No SDK framework. If a dependency is genuinely needed, flag it first.

## Architecture

Planned layout (Go, package `zonneplan`):

- `client.go` - HTTP client: request handling, retries, error mapping, context/timeout support.
- `auth.go` - OAuth2 authorization-code flow with PKCE and emailed one-time password; token refresh with expiry margin; `Token` type with save/restore for config persistence. Login is two-step (`RequestOTP(email)` -> `SubmitOTP(otp)`): the API mails the OTP, so the caller owns the prompts. That lets the SAME flow serve the interactive evcc web UI and a local CLI.
- Field alignment with `oauth2.Token` (access_token, refresh_token, expires_in) so the evcc wrapper can map tokens 1:1.
- `prices.go` - consumer price chart endpoints and models (`ConsumerPrices`, `PricePoint`, `Money`).
- `errors.go`, `const.go` - sentinel errors and API constants (base URL, app version headers, money factor).
- `cmd/zonneplan/` - small CLI for the interactive login (obtains token once; refresh token is then reusable non-interactively).

### API facts (from pyzonneplan, keep in sync)

- Base: `https://app-api.zonneplan.nl`. Requests need `x-app-version` and `x-app-environment: production` headers or the API rejects them.
- Auth is a three-call flow; the first call EXPECTS HTTP 403 with `otp_required: true` and an `auth_session` - that is the success path, not an error. Second call exchanges the emailed OTP for an `authorization_code`; third call exchanges code + PKCE verifier at `/oauth/token` for access/refresh tokens. Refresh rotates the refresh token, so concurrent refreshes must be serialized (singleflight or mutex).
- Alternative rate source: `GET /connections/{uuid}/summary` returns `data.price_per_hour[]` (~2 days ahead): keys `datetime` (RFC3339 with `.000Z`), flat-int `electricity_price`/`electricity_price_excl_tax` (NOT nested `Money`), `tariff_group`, `sustainability_score`. Deferred but worth knowing.
- Every monetary `amount` is an integer in 1e-7 EUR (`MONEY_FACTOR = 0.0000001`). Model as `type Money int64`.
- `price_tax_included`/`price_tax_excluded` differ by the FIXED energy tax (energiebelasting): electricity 0.1108481 EUR/kWh, gas 0.7267981 EUR/m3 (2026 rates, constant across all intervals). price_tax_included IS the all-in consumption price; price_tax_excluded is the feed-in base. Do not add any tax factor on top of either.
- Price endpoints: `api/consumer-prices/charts/{chart}` (general) and `api/connections/{uuid}/consumer-prices/charts/{chart}` (per contract). Chart names: `electricity-hourly`, `electricity-quarter-hourly`, `gas-daily`. Response envelope: `data.chart.series.prices[]`, each with `start_date`/`end_date` (ISO 8601, `+00:00`), `price_tax_included.amount`, `price_tax_excluded.amount`, optional `tariff_group` (hourly only) and `sustainability_score.permille`.
- Only GET requests may be retried (connection errors/timeouts); POST/PUT must never be retried (could apply an action twice - relevant once we add non-GET calls).

### evcc token rotation support

- evcc has ready-made token rotation: `evcc/util/oauth` (`refreshtokensource.go`, `persistenttokensource.go`). `oauth.RefreshTokenSource(log, *oauth2.Token, refresher func(*oauth2.Token) (*oauth2.Token, error))` returns an `oauth2.TokenSource` that refreshes under a mutex and carries over the old refresh token when the API does not rotate. `oauth2.NewClient(ctx, ts)` then gives an auto-refreshing `*http.Client`. `PersistentTokenSource` additionally persists to evcc settings and flushes immediately because a lost rotated token locks the user out.
- The standalone module must implement its own equivalent rotation (mutex + expiry margin, persist after refresh). The evcc wrapper plugs this module's refresher into `util/oauth.RefreshTokenSource` instead of duplicating it; see vehicle/bluelink/identity.go:230 for the idiom.

### Interactive login in the evcc web UI (flow C: challenge)

- evcc's provider-auth contract fits the OTP flow exactly: `api.AuthProvider` + `api.AuthChallenger` (api/api.go:230-261). `StartChallenge()` = POST authorize-challenge with the email (template auth param), which mails the OTP, and returns `{Kind: AuthChallengeCode}`; `SubmitChallenge(answer)` = exchange OTP + PKCE for tokens.
- Vendor-specific auth type goes in `evcc/plugin/auth/` (there is no zonneplan one yet; oauth.go is the generic redirect one, demo.go shows the shapes). The template opts in with an `auth` block; the auth check endpoint instantiates the token source and lists the provider in the Authorization Status card.
- The auth provider must persist the rotated token immediately (settings flush right away, see persistenttokensource.go store()); the token lives in the settings table under the provider id, NOT in the device config.
- Devices do not block on login: the tariff asks the token source on each poll and reports login-required until the user connects in the UI. No restart involved.

### evcc integration contract (the wrapper lives in ../evcc/tariff/)

- This module must NOT import evcc. It exposes a plain client returning typed slots (start/end `time.Time` + value in EUR/kWh). The evcc-side wrapper owns everything evcc-specific.
- Resolution: use the quarter-hourly chart (`electricity-quarter-hourly`) for evcc. It carries no `tariff_group` (hourly only) but matches evcc's 15m SlotDuration exactly. Prefer the connection-scoped endpoint (the user's own contract prices, as the app shows) with a fallback to the general endpoint.
- evcc types the wrapper maps to (api/rates.go:10-23): `api.Rate{Start, End time.Time, Value float64}`; `api.Rates` must be sorted by Start; evcc's `SlotDuration` is 15m. The wrapper must emit times via `.Local()` (see evcc tariff/frankenergie.go).
- Wrapper pattern (evcc tariff/frankenergie.go is canonical): struct with `*embed`, `*util.Logger`, `loc *time.Location`, `data *util.Monitor[api.Rates]`; `registry.Add("zonneplan", NewZonneplanFromConfig)`; factory decodes config then `runOrError(t)`; run loop uses `backoff.Retry` + `backoffPermanentError` + `reportError(&once, done, err)` + `mergeAfter`. Poll hourly, fetch today and tomorrow (day-ahead publishes around 13:00), merge keeping old future slots on failure.
- Precedent: evcc's go.mod already imports standalone vendor client modules (go-ecoflow, mazda-go, solarman-go, mbmd). A thin client module + thin tariff/ wrapper matches house style.
- Config UI: a template YAML at evcc templates/definition/tariff/ (see frank-energie.yaml; the `render:` block's `type:` must equal the registry name). Optional, decide in Phase 5.

### Feed-in vs feed-out (Zonnebonus)

- The consumer-prices charts are CONSUMPTION prices (marktprijs + inkoopvergoeding + energy tax + VAT). There is NO separate feed-in chart: the API exposes one price per interval.
- Feed-in (teruglevering) base is `PriceTaxExcluded`: Zonneplan's inkoop- and verkoopvergoeding are both 2 ct/kWh, so the markup already in the exclusive price matches the feed-in surcharge. No surcharge math.
- Zonnebonus: 10% on top of the full feed-in compensation, `(marktprijs + 0.02) x 1.10`, ONLY between astronomical sunrise and sunset (per address, KNMI times, quarters rounded in the customer's favor), only when the base is positive, max 7500 kWh/year (after which only the flat 0.02 remains), not for Zonneplan home batteries (Powerplay rates).
- The account response carries `address.sunrise`/`address.sunset` (RFC3339), so the bonus window can be computed exactly as the app does.
- Multiple addresses: `Account.ElectricityGroups()`, `MatchAddress(text)` and `AddressGroup.ElectricityUUIDs()` resolve them; the chosen group provides the connection uuid AND its sun times. evcc exposes an `address` param for this.
- evcc wrapper: config `tariffDirection: import|export` (see frank-energie); export mode derives rates from the same quarter-hourly chart: export = `FeedInRateInclusive` (Zonnebonus + EB until 2027, auto-drops after). evcc's tariff-features params (charges/tax/formula) can carry the adjustment.

## Gotchas

- The API's timestamps are UTC ISO 8601 with `+00:00` offset; the chart `range` uses `+02:00` (Europe/Amsterdam). Always use timezone-aware `time.Time` and compare with the API timezone `Europe/Amsterdam` for day boundaries.
- Do not invent credentials or endpoints; verify against pyzonneplan or real traffic.
- Refresh token rotation: persist the new refresh token after every refresh, or the next refresh fails.
