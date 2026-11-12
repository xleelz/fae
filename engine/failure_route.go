package engine

type FailureRoute struct {
	Stage   string `json:"stage"`
	Visit   int    `json:"visit"`
	Branch  string `json:"branch,omitempty"`
	Reason  string `json:"reason"`
	Detail  string `json:"error"`
	Handler string `json:"handler,omitempty"`
}
