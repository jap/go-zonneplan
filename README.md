# go-zonneplan

A Go client for the Zonneplan API (app-api.zonneplan.nl), focused on
rates. Inspired by pyzonneplan, built to be wrapped as an evcc tariff
provider: [evcc](https://github.com/evcc-io/evcc) is the primary
consumer.

Module path: `github.com/jap/go-zonneplan`. Dependencies: stdlib plus
testify for tests.

## Usage

```go
ctx := context.Background()
c := zonneplan.NewClient(zonneplan.WithEmail("you@example.com"))

// login: Zonneplan mails a one-time password to the account address
challenge, err := c.RequestOTP(ctx, "myapp")
if err != nil {
	log.Fatal(err)
}
// the user reads the code from the mail
if err := c.SubmitOTP(ctx, challenge, "123456"); err != nil {
	log.Fatal(err)
}

prices, err := c.ConsumerPrices(ctx, zonneplan.ChartElectricityHourly, "")
if err != nil {
	log.Fatal(err)
}
if p := prices.PriceAt(time.Now()); p != nil {
	log.Printf("current price: EUR/kWh %.7f",
		p.PriceTaxIncluded.Euros())
}
```

The token rotates on every refresh. Persist it (plain JSON) and
restore with `SetToken`.

## Rate formulas

Per interval, all EUR/kWh:

    incl = PriceTaxIncluded    all-in consumption price
    excl = PriceTaxExcluded    feed-in base
    EB   = incl - excl         energiebelasting, 0.1108481 in 2026

    AllIn()             = incl
    FeedInRate(sr, ss)  = excl * 1.10  if sun up and excl > 0
                        = excl         otherwise
    FeedInRateInclusive = FeedInRate + EB   until salderingEnd
                        = FeedInRate        from 2027 (saldering ended)

salderingEnd is Amsterdam midnight of 1 Jan 2027, fixed as
2026-12-31T23:00Z (quarters, so the fixed instant is exact).

Addresses: an account can have electricity connections at several
addresses. `Account.ElectricityGroups()`, `MatchAddress(text)` and
`AddressGroup.ElectricityUUIDs()` resolve them; the selected group
provides both the connection uuid and its sun times.

Sun up: the interval overlaps [sunrise, sunset) from the account
response, applied as a time of day to every day of the chart. The API
only returns the sun times for today, so yesterday and tomorrow get
the same pair; the daily drift of about two minutes is an accepted
deviation. evcc could keep a record of historical sun times and apply
the correct pair per day. Not modeled: the 7500 kWh/year Zonnebonus
cap and battery exclusions.

## Timezones

The API sends price points as UTC instants (`+00:00`), the chart range
and the account sun times in `Europe/Amsterdam` local offset; all are
parsed into timezone-aware `time.Time`. The CLI prints everything in
the machine's local timezone. evcc expects rates in local time too,
so the wrapper there maps the instants with `.Local()`.

## CLI

```
go run ./cmd/zonneplan login -email you@example.com
     [-token-file token.json]
```

Prompts for the emailed one-time password once; the saved token is
reused afterwards. If rejected, delete the token file and log in
again.

```
go run ./cmd/zonneplan prices [-token-file token.json]
     [-chart electricity-hourly] [-address text] [-connection uuid]
```

Prints contract prices: all-in, excl, feed-in estimate (sun marker
inside the Zonnebonus window), tariff group, sustainability. The chart
defaults to `electricity-quarter-hourly`; `-chart electricity-hourly`
is the only chart with a tariff group (low/normal/high).
`-connection` overrides the auto-discovered uuid; with several
addresses, `-address` picks one (a part of street or city).

## Status

Rates only. There is no evcc integration yet; a PR to
[evcc](https://github.com/evcc-io/evcc) (tariff provider plus login
flow) will follow.
