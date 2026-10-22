package daemon

type AdapterRetireParams struct {
	ID                 string `json:"id"`
	Reason             string `json:"reason"`
	AuthorizingMessage string `json:"authorizing_message"`
}
