// Package zonneplan is a client for the Zonneplan API (app-api.zonneplan.nl).
//
// Login is an OAuth2 authorization-code flow with PKCE where the user-facing
// step is a one-time password mailed to the account address instead of a
// browser redirect. See RequestOTP and SubmitOTP.
package zonneplan

const (
	// Version is the module version, reported as the user agent to the API.
	Version = "0.1.0"

	// APIURL is the base url of the Zonneplan app API.
	APIURL = "https://app-api.zonneplan.nl/"

	// appVersion and appEnvironment are sent by the mobile app; the API
	// rejects requests without them.
	appVersion     = "6.23.0"
	appEnvironment = "production"

	// MoneyFactor converts the API's integer amounts to euro: the unit is 1e-7 EUR.
	MoneyFactor = 1e-7
)

// Supported consumer price charts.
const (
	ChartElectricityHourly        = "electricity-hourly"
	ChartElectricityQuarterHourly = "electricity-quarter-hourly"
	ChartGasDaily                 = "gas-daily"
)
