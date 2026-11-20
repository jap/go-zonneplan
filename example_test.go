package zonneplan_test

import (
	"context"
	"log"
	"time"

	zonneplan "github.com/jap/go-zonneplan"
)

// Login once with the mailed one-time password, then poll rates. Persist the
// token between runs: it rotates on every refresh.
func Example() {
	ctx := context.Background()
	c := zonneplan.NewClient(zonneplan.WithEmail("you@example.com"))

	challenge, err := c.RequestOTP(ctx, "myapp")
	if err != nil {
		log.Fatal(err)
	}
	// the user reads the code from the mail zonneplan just sent
	if err := c.SubmitOTP(ctx, challenge, "123456"); err != nil {
		log.Fatal(err)
	}

	prices, err := c.ConsumerPrices(ctx, zonneplan.ChartElectricityHourly, "")
	if err != nil {
		log.Fatal(err)
	}
	if p := prices.PriceAt(time.Now()); p != nil {
		log.Printf("current price: EUR/kWh %.7f", p.PriceTaxIncluded.Euros())
	}
}
