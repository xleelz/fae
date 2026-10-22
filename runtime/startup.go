package runtime

import "time"

func startupLimit(value time.Duration) time.Duration {
	if value <= 0 {
		return 180 * time.Second
	}
	return value
}
