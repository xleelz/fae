package workflow

// FailureRouteReport cites the evaluations that caused a recorded failure edge.
// HandlerPassed is absent while its outcome is still unknown.
type FailureRouteReport struct {
	Stage         string   `json:"stage"`
	Visit         int      `json:"visit"`
	Branch        string   `json:"branch,omitempty"`
	Reason        string   `json:"reason"`
	Detail        string   `json:"error,omitempty"`
	Handler       string   `json:"handler"`
	HandlerPassed *bool    `json:"handler_passed,omitempty"`
	Records       []string `json:"records,omitempty"`
}
type RefusalReport struct {
	Stage     string `json:"stage"`
	Visit     int    `json:"visit"`
	Branch    string `json:"branch,omitempty"`
	Reason    string `json:"reason"`
	Detail    string `json:"error"`
	Handler   string `json:"handler,omitempty"`
	Escalated bool   `json:"escalated"`
}
