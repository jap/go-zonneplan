package zonneplan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Connection is an installation at an address, e.g. the electricity or gas
// meter connection a contract belongs to.
type Connection struct {
	UUID          string `json:"uuid"`
	MarketSegment string `json:"market_segment"`
}

// Address is the address an address group belongs to, including the sun
// times Zonneplan computes for it (the Zonnebonus window).
type Address struct {
	Street   string    `json:"street"`
	Number   string    `json:"number"`
	Addition string    `json:"addition"`
	City     string    `json:"city"`
	Sunrise  time.Time `json:"sunrise"`
	Sunset   time.Time `json:"sunset"`
}

// String returns the address as "street number, city".
func (a Address) String() string {
	number := a.Number
	if a.Addition != "" {
		number += " " + a.Addition
	}
	return strings.TrimSpace(a.Street + " " + number + ", " + a.City)
}

// ElectricityUUIDs returns the uuids of the group's electricity connections.
func (g AddressGroup) ElectricityUUIDs() []string {
	var uuids []string
	for _, conn := range g.Connections {
		if conn.MarketSegment == "electricity" {
			uuids = append(uuids, conn.UUID)
		}
	}
	return uuids
}

// AddressGroup groups the connections at one address.
type AddressGroup struct {
	UUID        string       `json:"uuid"`
	Connections []Connection `json:"connections"`
	Address     Address      `json:"address"`
}

// Account is the authenticated user's account.
type Account struct {
	UUID          string
	Email         string
	FullName      string
	AddressGroups []AddressGroup
}

// ElectricityConnectionUUIDs returns the uuid of every electricity connection,
// the ones contract-scoped consumer prices can be fetched for.
func (a *Account) ElectricityConnectionUUIDs() []string {
	var uuids []string
	for _, group := range a.ElectricityGroups() {
		uuids = append(uuids, group.ElectricityUUIDs()...)
	}
	return uuids
}

// ElectricityGroups returns the address groups that have electricity
// connections, the ones contract-scoped consumer prices can be fetched for.
func (a *Account) ElectricityGroups() []AddressGroup {
	var groups []AddressGroup
	for _, group := range a.AddressGroups {
		if len(group.ElectricityUUIDs()) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// MatchAddress finds the address group matching the given text: a
// case-insensitive substring match on street, number, city and zipcode,
// ignoring punctuation. Use the Address.String() form of the options.
func (a *Account) MatchAddress(text string) (AddressGroup, bool) {
	want := normalize(text)
	for _, group := range a.AddressGroups {
		if strings.Contains(normalize(group.Address.String()), want) {
			return group, true
		}
	}
	return AddressGroup{}, false
}

// normalize makes a copied address option match the API's format.
//
// When the account has electricity connections at several addresses, the
// first fetch fails with an error naming the options, for example
// "Teststraat 1, Voorbeeldstad; Dorpsstraat 2 a, Ambachtstad". The user
// copies one of these options, or any distinctive fragment of one (a
// street or city name is enough), into the address parameter of the evcc
// tariff or the -address flag of the CLI. normalize makes that pasted
// text match how the API formats the address: case, punctuation and
// spacing collapse away.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// GetAccount fetches the authenticated Zonneplan account, its addresses and
// connections.
func (c *Client) GetAccount(ctx context.Context) (*Account, error) {
	raw, err := c.do(ctx, http.MethodGet, "user-accounts/me", nil, nil, requestOpts{authenticated: true})
	if err != nil {
		return nil, err
	}

	var res struct {
		Data struct {
			UserAccount struct {
				UUID     string `json:"uuid"`
				Email    string `json:"email"`
				FullName string `json:"full_name"`
			} `json:"user_account"`
			AddressGroups []AddressGroup `json:"address_groups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("%w: unexpected account response: %v", ErrResponse, err)
	}

	return &Account{
		UUID:          res.Data.UserAccount.UUID,
		Email:         res.Data.UserAccount.Email,
		FullName:      res.Data.UserAccount.FullName,
		AddressGroups: res.Data.AddressGroups,
	}, nil
}
