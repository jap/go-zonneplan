package zonneplan

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAccount(t *testing.T) {
	var path string
	c := serveFixture(t, "get_account.json", func(r *http.Request) {
		path = r.URL.Path
	})

	account, err := c.GetAccount(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/user-accounts/me", path)

	assert.Equal(t, "00000000-0000-4000-8000-000000000001", account.UUID)
	assert.Equal(t, "user@example.com", account.Email)
	assert.Equal(t, "Jane Doe", account.FullName)
	require.Len(t, account.AddressGroups, 1)
	require.Len(t, account.AddressGroups[0].Connections, 2)
}

func TestElectricityConnectionUUIDs(t *testing.T) {
	c := serveFixture(t, "get_account.json", nil)

	account, err := c.GetAccount(context.Background())
	require.NoError(t, err)

	// the fixture has one electricity and one gas connection
	assert.Equal(t, []string{"00000000-0000-4000-8000-000000000002"}, account.ElectricityConnectionUUIDs())
}

func TestAddressString(t *testing.T) {
	c := serveFixture(t, "get_account.json", nil)

	account, err := c.GetAccount(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "Teststraat 1, Voorbeeldstad", account.AddressGroups[0].Address.String())
}

func TestElectricityGroupsAndMatchAddress(t *testing.T) {
	c := serveFixture(t, "get_account_multi.json", nil)

	account, err := c.GetAccount(context.Background())
	require.NoError(t, err)

	// two groups with electricity, the second also carrying a gas connection? no: one each
	groups := account.ElectricityGroups()
	require.Len(t, groups, 2)
	assert.Equal(t, []string{"00000000-0000-4000-8000-000000000002"}, groups[0].ElectricityUUIDs())
	assert.Equal(t, []string{"00000000-0000-4000-8000-000000000097"}, groups[1].ElectricityUUIDs())

	g, ok := account.MatchAddress("dorpsstraat 2")
	require.True(t, ok)
	assert.Equal(t, "Dorpsstraat 2 a, Ambachtstad", g.Address.String())

	g, ok = account.MatchAddress("voorbeeld")
	require.True(t, ok)
	assert.Equal(t, "Teststraat 1, Voorbeeldstad", g.Address.String())

	_, ok = account.MatchAddress("kerkweg")
	assert.False(t, ok)
}

func TestElectricityConnectionUUIDsMultiple(t *testing.T) {
	c := serveFixture(t, "get_account_multi.json", nil)

	account, err := c.GetAccount(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000097",
	}, account.ElectricityConnectionUUIDs())
}

func TestSalderingEnd(t *testing.T) {
	// Amsterdam midnight of 1 Jan 2027, as a fixed UTC instant
	assert.True(t, salderingEnd.Equal(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)))
}

func TestGetAccountInvalidResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": {"address_groups": "nope"}}`))
	})

	_, err := c.GetAccount(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrResponse)
}
