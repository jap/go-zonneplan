package zonneplan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Money is a monetary amount, expressed in the API's 1e-7 EUR unit.
type Money int64

// Euros returns the amount in euro.
func (m Money) Euros() float64 { return float64(m) * MoneyFactor }

func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{Amount: int64(m)})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var v moneyJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*m = Money(v.Amount)
	return nil
}

type moneyJSON struct {
	Amount int64 `json:"amount"`
}

// SustainabilityScore is how sustainable the energy is during a price
// interval, in permille (0 to 1000).
type SustainabilityScore int

// Permille returns the raw score.
func (s SustainabilityScore) Permille() int { return int(s) }

func (s *SustainabilityScore) UnmarshalJSON(data []byte) error {
	var v struct {
		Permille int `json:"permille"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*s = SustainabilityScore(v.Permille)
	return nil
}

// PricePoint is a single price interval within a chart. TariffGroup is only
// present on the hourly electricity chart, not on the quarter-hourly one.
type PricePoint struct {
	StartDate        time.Time            `json:"start_date"`
	EndDate          time.Time            `json:"end_date"`
	PriceTaxIncluded Money                `json:"price_tax_included"`
	PriceTaxExcluded Money                `json:"price_tax_excluded"`
	TariffGroup      string               `json:"tariff_group,omitempty"`
	Sustainability   *SustainabilityScore `json:"sustainability_score,omitempty"`
}

// PriceRange is the date range a chart covers.
type PriceRange struct {
	StartDate time.Time `json:"start_date"`
	EndDate   time.Time `json:"end_date"`
}

// ConsumerPrices is a consumer price chart: the range it covers and its
// price points, in chronological order.
type ConsumerPrices struct {
	Range  PriceRange
	Prices []PricePoint
}

// ZonneBonusFactor is the factor Zonneplan adds to the feed-in compensation
// for solar fed in between sunrise and sunset.
const ZonneBonusFactor = 1.10

// EnergyTax returns the interval's energy tax (energiebelasting): the API's
// in/exclusive rates differ by exactly the fixed per-kWh tax (verified
// against the 2026 rates: 0.1108481 EUR/kWh electricity, 0.7267981 EUR/m3
// gas).
func (p PricePoint) EnergyTax() Money {
	return p.PriceTaxIncluded - p.PriceTaxExcluded
}

// salderingEnd is when the salderingsregeling ends: 1 January 2027, midnight
// Amsterdam time (Dec 31 23:00 UTC).
var salderingEnd = time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)

// FeedInRateInclusive estimates the feed-in rate as money received per kWh.
// While the salderingsregeling runs (until 2027) the energy tax comes back,
// so it is added; from salderingEnd it does not and the exclusive feed-in
// rate is the whole compensation.
func (p PricePoint) FeedInRateInclusive(sunrise, sunset time.Time) float64 {
	rate := p.FeedInRate(sunrise, sunset)
	if !p.StartDate.Before(salderingEnd) {
		return rate
	}
	return rate + p.EnergyTax().Euros()
}

// AllIn returns the actual consumption price per kWh. PriceTaxIncluded
// already includes the energy tax and VAT; no further application needed.
func (p PricePoint) AllIn() float64 {
	return p.PriceTaxIncluded.Euros()
}

// SunUp reports whether the interval overlaps the sun window. Zonneplan
// judges the bonus per quarter and rounds in the customer's favor, so an
// interval that merely touches the window counts as sun up.
//
// The account reports the window as one date's absolute timestamps; it is
// used as a time of day and re-anchored on the interval's own day.
func (p PricePoint) SunUp(sunrise, sunset time.Time) bool {
	if sunrise.IsZero() || sunset.IsZero() {
		return false
	}
	sr := clockOffset(sunrise, sunrise.Location())
	ss := clockOffset(sunset, sunrise.Location())
	start := clockOffset(p.StartDate, sunrise.Location())
	end := clockOffset(p.EndDate, sunrise.Location())
	if end <= start { // end of interval can be midnight
		end += 24 * time.Hour
	}
	return start < ss && end > sr
}

// clockOffset returns the time of day as a duration since midnight.
func clockOffset(t time.Time, loc *time.Location) time.Duration {
	t = t.In(loc)
	return time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
}

// FeedInRate estimates the feed-in (teruglevering) rate for this interval:
// the exclusive price as base, times the Zonnebonus factor when the sun is
// up (see SunUp). Zonneplan's inkoop- and verkoopvergoeding are both 2 ct,
// so the markup already in the exclusive price matches the feed-in
// surcharge. sunrise/sunset come from the account response and are applied
// as-is to every day of the chart: the sun times drift about two minutes per
// day, small enough to ignore. Pass zero times when unknown, which skips the
// bonus. The 7500 kWh/year cap and battery exclusions are billing-side and
// not reflected here.
func (p PricePoint) FeedInRate(sunrise, sunset time.Time) float64 {
	base := p.PriceTaxExcluded.Euros()
	if base <= 0 || !p.SunUp(sunrise, sunset) {
		return base
	}
	return base * ZonneBonusFactor
}

// PriceAt returns the price point whose [start, end) interval contains
// moment, or nil when the chart does not cover it. Works for hourly and
// quarter-hourly charts alike.
func (cp *ConsumerPrices) PriceAt(moment time.Time) *PricePoint {
	for i := range cp.Prices {
		p := &cp.Prices[i]
		if !p.StartDate.After(moment) && p.EndDate.After(moment) {
			return p
		}
	}
	return nil
}

// ConsumerPrices fetches a consumer price chart: one of the Chart* constants.
// With a connectionUUID, fetch the prices of that connection's contract
// instead of the general ones, as the app does.
func (c *Client) ConsumerPrices(ctx context.Context, chart string, connectionUUID string) (*ConsumerPrices, error) {
	if !validChart(chart) {
		return nil, fmt.Errorf("%w: unknown chart %q", ErrRequest, chart)
	}

	uri := "api/consumer-prices/charts/" + chart
	if connectionUUID != "" {
		uri = "api/connections/" + connectionUUID + "/consumer-prices/charts/" + chart
	}

	raw, err := c.do(ctx, http.MethodGet, uri, nil, nil, requestOpts{authenticated: true})
	if err != nil {
		return nil, err
	}

	var res struct {
		Data struct {
			Chart struct {
				Range  PriceRange `json:"range"`
				Series struct {
					Prices []PricePoint `json:"prices"`
				} `json:"series"`
			} `json:"chart"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("%w: unexpected consumer prices response: %v", ErrResponse, err)
	}

	return &ConsumerPrices{
		Range:  res.Data.Chart.Range,
		Prices: res.Data.Chart.Series.Prices,
	}, nil
}

func validChart(chart string) bool {
	switch chart {
	case ChartElectricityHourly, ChartElectricityQuarterHourly, ChartGasDaily:
		return true
	}
	return false
}
