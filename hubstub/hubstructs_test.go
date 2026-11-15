package hubstub

// Copies of the structs the hub decodes the stub's answers into (getAlby/hub v1.24.7), so a test decodes each response
// the way the hub does, strictly: unknown fields are refused here, which the hub would ignore.

// alby/alby_service.go:199-219 (the wire form of GET /api/internal/info, decoded by GetInfo).
type hubInfoHub struct {
	LatestVersion      string `json:"latest_version"`
	LatestReleaseNotes string `json:"latest_release_notes"`
	SwapProvider       string `json:"swap_provider"`
}

type hubInfoIncident struct {
	Name    string `json:"name"`
	Started string `json:"started"`
	Status  string `json:"status"`
	Impact  string `json:"impact"`
	Url     string `json:"url"`
}

type hubInfo struct {
	Hub              hubInfoHub        `json:"hub"`
	Status           string            `json:"status"`
	Healthy          bool              `json:"healthy"`
	AccountAvailable bool              `json:"account_available"`
	Incidents        []hubInfoIncident `json:"incidents"`
}

// alby/models.go:108-131 (GET /api/internal/channel_suggestions, decoded by GetChannelPeerSuggestions).
type hubChannelPeerSuggestion struct {
	Network                    string  `json:"network"`
	PaymentMethod              string  `json:"paymentMethod"`
	Pubkey                     string  `json:"pubkey"`
	Host                       string  `json:"host"`
	MinimumChannelSize         uint64  `json:"minimumChannelSize"`
	MinimumChannelSizeSat      uint64  `json:"minimumChannelSizeSat"`
	MaximumChannelSize         uint64  `json:"maximumChannelSize"`
	MaximumChannelSizeSat      uint64  `json:"maximumChannelSizeSat"`
	MaximumChannelExpiryBlocks *uint32 `json:"maximumChannelExpiryBlocks"`
	Name                       string  `json:"name"`
}

// alby/models.go:144-149 (GET /api/rates: a map of currencies, decoded by GetCurrencies).
type hubCurrency struct {
	IsoCode  string `json:"iso_code"`
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

// alby/models.go:169-175 (GET /api/stories, decoded by GetStories in alby/alby_oauth_service.go).
type hubStory struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Avatar  string `json:"avatar"`
	VideoID string `json:"videoId,omitempty"`
}
