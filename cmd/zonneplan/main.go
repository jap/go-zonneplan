// Command zonneplan performs an interactive login against the Zonneplan API
// and dumps consumer price charts. The token rotates on every refresh, so
// keep using the same -token-file: it is rewritten in place.
//
// Usage:
//
//	zonneplan login -email you@example.com [-token-file token.json]
//	zonneplan prices [-token-file token.json] [-chart electricity-hourly] [-connection uuid]
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	zp "github.com/jap/go-zonneplan"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()

	switch os.Args[1] {
	case "login":
		login(ctx, os.Args[2:])
	case "prices":
		prices(ctx, os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  zonneplan login -email you@example.com [-token-file token.json]
  zonneplan prices [-token-file token.json] [-chart electricity-quarter-hourly] [-connection uuid]`)
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	fmt.Fprintln(os.Stderr, "if the token was rejected, delete the token file and log in again")
	os.Exit(1)
}

func login(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	email := fs.String("email", "", "zonneplan account email")
	tokenFile := fs.String("token-file", "token.json", "token storage, refreshed in place")
	if err := fs.Parse(args); err != nil {
		fatal(err)
	}
	if *email == "" {
		fmt.Fprintln(os.Stderr, "login requires -email")
		os.Exit(2)
	}

	c := zp.NewClient(zp.WithEmail(*email))
	if t := loadToken(*tokenFile); t != nil {
		c.SetToken(t)
		fmt.Println("restored token from", *tokenFile)
	}

	if !c.Token().Valid() {
		challenge, err := c.RequestOTP(ctx, "go-zonneplan")
		if err != nil {
			fatal(err)
		}
		fmt.Printf("one-time password mailed to %s, enter it: ", *email)

		otp, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			fatal(err)
		}
		if err := c.SubmitOTP(ctx, challenge, strings.TrimSpace(otp)); err != nil {
			fatal(err)
		}
	}

	saveToken(*tokenFile, c.Token())
	fmt.Println("token written to", *tokenFile)
}

func prices(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("prices", flag.ExitOnError)
	tokenFile := fs.String("token-file", "token.json", "token storage, refreshed in place")
	chart := fs.String("chart", zp.ChartElectricityQuarterHourly, "electricity-quarter-hourly, electricity-hourly or gas-daily")
	connection := fs.String("connection", "", "connection uuid for contract-scoped prices")
	address := fs.String("address", "", "address of the connection, when the account has several")
	if err := fs.Parse(args); err != nil {
		fatal(err)
	}

	c := zp.NewClient()
	if t := loadToken(*tokenFile); t == nil {
		fmt.Fprintln(os.Stderr, "no token in", *tokenFile, "- run: zonneplan login -email you@example.com")
		os.Exit(1)
	} else {
		c.SetToken(t)
	}

	// the account provides the connection uuid and the Zonnebonus sun window
	var sunrise, sunset time.Time
	account, err := c.GetAccount(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: no account (no contract prices, no zonnebonus window): %v\n", err)
	} else {
		group, err := resolveGroup(account, *connection, *address)
		if err != nil {
			fatal(err)
		}
		if *connection == "" {
			*connection = group.ElectricityUUIDs()[0]
		}
		sunrise, sunset = group.Address.Sunrise, group.Address.Sunset
	}

	// contract-scoped prices, general ones only as fallback
	scoped, err := c.ConsumerPrices(ctx, *chart, *connection)
	if err != nil && *connection != "" {
		fmt.Fprintf(os.Stderr, "warning: no contract prices for connection %s: %v\n", *connection, err)
	}
	general := scoped
	if general == nil {
		general, err = c.ConsumerPrices(ctx, *chart, "")
		if err != nil {
			fatal(err)
		}
	}

	// ensureToken may have rotated the refresh token during the calls
	saveToken(*tokenFile, c.Token())

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	if scoped != nil {
		fmt.Fprintf(os.Stderr, "contract: connection %s\n", *connection)
		fmt.Fprintf(os.Stderr, "zonnebonus window (all days): %s - %s local\n", sunrise.Local().Format("15:04"), sunset.Local().Format("15:04"))
		fmt.Fprintln(w, "START\tEND\tCONTRACT EUR/KWH INCL.\tCONTRACT EUR/KWH EXCL.\tFEED-IN INCL. EST.\tTARIFF\tSUSTAINABILITY")
		for _, p := range scoped.Prices {
			fmt.Fprintf(w, "%s\t%s\t%.7f\t%.7f\t%s\t%s\t%s\n",
				startTime(p),
				endTime(p),
				p.PriceTaxIncluded.Euros(),
				p.PriceTaxExcluded.Euros(),
				feedInColumn(p, sunrise, sunset),
				p.TariffGroup,
				sustainability(p),
			)
		}
	} else {
		fmt.Fprintln(os.Stderr, "showing general prices only")
		fmt.Fprintf(os.Stderr, "zonnebonus window (all days): %s - %s local\n", sunrise.Local().Format("15:04"), sunset.Local().Format("15:04"))
		fmt.Fprintln(w, "START\tEND\tEUR/KWH INCL.\tEUR/KWH EXCL.\tFEED-IN INCL. EST.\tTARIFF\tSUSTAINABILITY")
		for _, p := range general.Prices {
			fmt.Fprintf(w, "%s\t%s\t%.7f\t%.7f\t%s\t%s\t%s\n",
				startTime(p),
				endTime(p),
				p.PriceTaxIncluded.Euros(),
				p.PriceTaxExcluded.Euros(),
				feedInColumn(p, sunrise, sunset),
				p.TariffGroup,
				sustainability(p),
			)
		}
	}
	if err := w.Flush(); err != nil {
		fatal(err)
	}
}

// resolveGroup picks the address group to use: by connection uuid, by address
// text, or the only group with electricity connections.
func resolveGroup(account *zp.Account, connection, address string) (zp.AddressGroup, error) {
	if connection != "" {
		for _, g := range account.AddressGroups {
			if slices.Contains(g.ElectricityUUIDs(), connection) {
				return g, nil
			}
		}
	}

	if address != "" {
		if g, ok := account.MatchAddress(address); ok {
			return g, nil
		}
		return zp.AddressGroup{}, fmt.Errorf("no address matches %q, available: %s", address, options(account))
	}

	groups := account.ElectricityGroups()
	switch len(groups) {
	case 1:
		return groups[0], nil
	case 0:
		return zp.AddressGroup{}, fmt.Errorf("the account has no electricity connection")
	default:
		return zp.AddressGroup{}, fmt.Errorf("multiple addresses, use -address with one of: %s", options(account))
	}
}

func options(account *zp.Account) string {
	var opts []string
	for _, g := range account.ElectricityGroups() {
		opts = append(opts, g.Address.String())
	}
	return strings.Join(opts, "; ")
}

// startTime and endTime format the interval bounds as local clock times.
func startTime(p zp.PricePoint) string {
	return p.StartDate.Local().Format("2006-01-02 15:04")
}

func endTime(p zp.PricePoint) string {
	return p.EndDate.Local().Format("15:04")
}

// feedInColumn formats the estimated feed-in rate including energy tax and
// VAT, with a sun marker when the interval overlaps the Zonnebonus window.
func feedInColumn(p zp.PricePoint, sunrise, sunset time.Time) string {
	rate := fmt.Sprintf("%.7f", p.FeedInRateInclusive(sunrise, sunset))
	if p.SunUp(sunrise, sunset) {
		rate += " \u2600\ufe0f"
	}
	return rate
}

func sustainability(p zp.PricePoint) string {
	if p.Sustainability != nil {
		return fmt.Sprintf("%d\u2030", p.Sustainability.Permille())
	}
	return ""
}

func loadToken(file string) *zp.Token {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var t zp.Token
	if json.Unmarshal(data, &t) != nil {
		return nil
	}
	return &t
}

func saveToken(file string, t *zp.Token) {
	if t == nil {
		return
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		fatal(err)
	}
}
