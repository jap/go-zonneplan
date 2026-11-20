package zonneplan

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveFixture serves a testdata file for every request, with a request hook.
func serveFixture(t *testing.T, file string, hook func(*http.Request)) *Client {
	t.Helper()
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(r)
		}
		data, err := os.ReadFile(filepath.Join("testdata", file))
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
}

func TestConsumerPricesHourly(t *testing.T) {
	var path string
	c := serveFixture(t, "get_consumer_prices_electricity_hourly.json", func(r *http.Request) {
		path = r.URL.Path
	})

	prices, err := c.ConsumerPrices(context.Background(), ChartElectricityHourly, "")
	require.NoError(t, err)
	assert.Equal(t, "/api/consumer-prices/charts/electricity-hourly", path)

	assert.True(t, prices.Range.StartDate.Equal(time.Date(2026, 8, 28, 15, 0, 0, 0, amsterdam())))
	assert.True(t, prices.Range.EndDate.Equal(time.Date(2026, 8, 30, 23, 59, 59, 0, amsterdam())))
	require.NotEmpty(t, prices.Prices)

	first := prices.Prices[0]
	assert.True(t, first.StartDate.Equal(time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)))
	assert.Equal(t, int64(2604541), int64(first.PriceTaxIncluded))
	assert.InDelta(t, 0.2604541, first.PriceTaxIncluded.Euros(), 1e-9)
	assert.InDelta(t, 0.1496060, first.PriceTaxExcluded.Euros(), 1e-9)
	assert.Equal(t, "normal", first.TariffGroup)
	require.NotNil(t, first.Sustainability)
	assert.Equal(t, 1000, first.Sustainability.Permille())

	// chronological order, contiguous hourly steps
	prev := prices.Prices[0]
	for _, p := range prices.Prices[1:] {
		assert.True(t, p.StartDate.Equal(prev.EndDate), "points must be contiguous")
		assert.Equal(t, time.Hour, p.StartDate.Sub(prev.StartDate))
		prev = p
	}
}

func TestConsumerPricesQuarterHourly(t *testing.T) {
	var path string
	c := serveFixture(t, "get_consumer_prices_connection_quarter_hourly.json", func(r *http.Request) {
		path = r.URL.Path
	})
	const uuid = "abc-connection-uuid"

	prices, err := c.ConsumerPrices(context.Background(), ChartElectricityQuarterHourly, uuid)
	require.NoError(t, err)
	assert.Equal(t, "/api/connections/"+uuid+"/consumer-prices/charts/electricity-quarter-hourly", path)

	require.NotEmpty(t, prices.Prices)
	assert.True(t, prices.Prices[0].StartDate.Equal(time.Date(2026, 10, 12, 7, 0, 0, 0, time.UTC)))

	for _, p := range prices.Prices {
		assert.Empty(t, p.TariffGroup, "quarter-hourly points carry no tariff group")
		assert.Equal(t, 15*time.Minute, p.EndDate.Sub(p.StartDate))
	}
}

func TestConsumerPricesGasDaily(t *testing.T) {
	c := serveFixture(t, "get_consumer_prices_gas_daily.json", nil)

	prices, err := c.ConsumerPrices(context.Background(), ChartGasDaily, "")
	require.NoError(t, err)

	require.NotEmpty(t, prices.Prices)
	first := prices.Prices[0]
	assert.True(t, first.StartDate.Equal(time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)), "gas day starts 04:00Z")
	assert.Equal(t, 24*time.Hour, first.EndDate.Sub(first.StartDate))
	assert.InDelta(t, 1.6029802, first.PriceTaxIncluded.Euros(), 1e-9)
}

func TestConsumerPricesUnknownChart(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { requests++ })

	_, err := c.ConsumerPrices(context.Background(), "electricity-biweekly", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequest)
	assert.Equal(t, 0, requests, "must reject before touching the network")
}

func TestConsumerPricesInvalidResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": {"chart": {"series": {"prices": "nope"}}}}`))
	})

	_, err := c.ConsumerPrices(context.Background(), ChartElectricityHourly, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrResponse)
}

func TestConsumerPricesUnauthenticated(t *testing.T) {
	// the general endpoint needs no connection uuid; auth still applies
	var auth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data": {"chart": {"range": {}, "series": {"prices": []}}}}`))
	})
	c.SetToken(&Token{AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour)})

	_, err := c.ConsumerPrices(context.Background(), ChartElectricityHourly, "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(auth, "Bearer "))
}

func TestPriceAt(t *testing.T) {
	c := serveFixture(t, "get_consumer_prices_electricity_hourly.json", nil)
	prices, err := c.ConsumerPrices(context.Background(), ChartElectricityHourly, "")
	require.NoError(t, err)

	p := prices.PriceAt(time.Date(2026, 8, 28, 13, 30, 0, 0, time.UTC))
	require.NotNil(t, p)
	assert.Equal(t, int64(2604541), int64(p.PriceTaxIncluded))

	assert.Nil(t, prices.PriceAt(time.Date(2026, 8, 28, 12, 59, 0, 0, time.UTC)))
	assert.Nil(t, prices.PriceAt(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)))
}

func TestMoneyJSONRoundTrip(t *testing.T) {
	in := Money(2604541)

	data, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount": 2604541}`, string(data))

	var out Money
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, in, out)
}

func TestSustainabilityScoreJSON(t *testing.T) {
	var s SustainabilityScore
	require.NoError(t, json.Unmarshal([]byte(`{"permille": 254}`), &s))
	assert.Equal(t, 254, s.Permille())
}

func TestMoneyRejectsInvalidShape(t *testing.T) {
	var m Money
	err := json.Unmarshal([]byte(`"2604541"`), &m)
	require.Error(t, err)
}

func TestAllIn(t *testing.T) {
	// the inclusive rate IS the all-in rate: VAT already included
	p := PricePoint{PriceTaxIncluded: Money(2604541)}
	assert.InDelta(t, 0.2604541, p.AllIn(), 1e-9)
}

func TestEnergyTax(t *testing.T) {
	// in/exclusive differ by the fixed energy tax: 2026 electricity rate
	p := PricePoint{PriceTaxIncluded: Money(2604541), PriceTaxExcluded: Money(1496060)}
	assert.Equal(t, Money(1108481), p.EnergyTax())

	// gas has its own constant: 0.7267981 EUR/m3
	gas := PricePoint{PriceTaxIncluded: Money(16029802), PriceTaxExcluded: Money(8761816)}
	assert.Equal(t, Money(7267986), gas.EnergyTax())
}

func TestFeedInRateInclusive(t *testing.T) {
	sunrise := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
	sunset := time.Date(2026, 8, 28, 19, 0, 0, 0, time.UTC)

	// excl EUR 2.00, EB EUR 1.20: feed-in excl 2.20 in sun, 2.00 outside;
	// inclusive: feed-in excl + 1.20, no VAT
	inSun := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC),
		PriceTaxIncluded: Money(32000000),
		PriceTaxExcluded: Money(20000000),
	}
	assert.InDelta(t, 3.4, inSun.FeedInRateInclusive(sunrise, sunset), 1e-9)

	outside := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 21, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 22, 0, 0, 0, time.UTC),
		PriceTaxIncluded: Money(32000000),
		PriceTaxExcluded: Money(20000000),
	}
	assert.InDelta(t, 3.2, outside.FeedInRateInclusive(sunrise, sunset), 1e-9)

	// from 2027 the salderingsregeling has ended: no energy tax refund, even
	// in the sun. Hard to see in real charts (they span ~3 days), so use
	// synthetic intervals around the boundary.
	afterSaldering := PricePoint{
		StartDate:        time.Date(2027, 1, 1, 13, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2027, 1, 1, 14, 0, 0, 0, time.UTC),
		PriceTaxIncluded: Money(32000000),
		PriceTaxExcluded: Money(20000000),
	}
	assert.InDelta(t, 2.2, afterSaldering.FeedInRateInclusive(sunrise, sunset), 1e-9)

	justBefore := PricePoint{
		StartDate:        salderingEnd.Add(-time.Hour),
		EndDate:          salderingEnd,
		PriceTaxIncluded: Money(32000000),
		PriceTaxExcluded: Money(20000000),
	}
	assert.InDelta(t, 3.2, justBefore.FeedInRateInclusive(sunrise, sunset), 1e-9)
}

// amsterdam is the API's local timezone.
func amsterdam() *time.Location {
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		panic(err)
	}
	return loc
}

func TestFeedInRate(t *testing.T) {
	sunrise := time.Date(2026, 8, 28, 5, 0, 0, 0, time.UTC)
	sunset := time.Date(2026, 8, 28, 19, 0, 0, 0, time.UTC)

	positiveInSun := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC),
		PriceTaxExcluded: Money(20000000), // EUR 2.00
	}
	positiveOutsideSun := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 21, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 22, 0, 0, 0, time.UTC),
		PriceTaxExcluded: Money(20000000),
	}
	negativeInSun := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC),
		PriceTaxExcluded: Money(-10000000), // EUR -1.00
	}

	// 10% bonus inside the sun window only, and only on a positive base
	assert.InDelta(t, 2.2, positiveInSun.FeedInRate(sunrise, sunset), 1e-9)
	assert.InDelta(t, 2.0, positiveOutsideSun.FeedInRate(sunrise, sunset), 1e-9)
	assert.InDelta(t, -1.0, negativeInSun.FeedInRate(sunrise, sunset), 1e-9)

	// zero sun times: no bonus anywhere
	assert.InDelta(t, 2.0, positiveInSun.FeedInRate(time.Time{}, time.Time{}), 1e-9)

	// Zonneplan rounds in the customer's favor: an interval that merely
	// touches the window counts as sun up
	touchingEnd := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 18, 45, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 19, 45, 0, 0, time.UTC),
		PriceTaxExcluded: Money(20000000),
	}
	assert.True(t, touchingEnd.SunUp(sunrise, sunset))
	assert.InDelta(t, 2.2, touchingEnd.FeedInRate(sunrise, sunset), 1e-9)

	justAfter := PricePoint{
		StartDate:        time.Date(2026, 8, 28, 19, 0, 0, 0, time.UTC),
		EndDate:          time.Date(2026, 8, 28, 20, 0, 0, 0, time.UTC),
		PriceTaxExcluded: Money(20000000),
	}
	assert.False(t, justAfter.SunUp(sunrise, sunset))
	assert.InDelta(t, 2.0, justAfter.FeedInRate(sunrise, sunset), 1e-9)

	// the window is a time of day: it applies on other days too, even though
	// the account reports it as absolute timestamps of one date
	windowTomorrow := PricePoint{
		StartDate:        sunrise.Add(-24 * time.Hour).Add(time.Hour),
		EndDate:          sunrise.Add(-24 * time.Hour).Add(2 * time.Hour),
		PriceTaxExcluded: Money(20000000),
	}
	assert.True(t, windowTomorrow.SunUp(sunrise, sunset))
	assert.InDelta(t, 2.2, windowTomorrow.FeedInRate(sunrise, sunset), 1e-9)
}
