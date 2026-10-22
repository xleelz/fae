package scheduler

// CanAcquireDynamic permits existing usage above a newly lowered limit, but
// never admits another holder until sufficient capacity has drained.
// @ requires limit > 0 && used >= 0
// @ ensures ok ==> weight > 0 && used + weight <= limit
func CanAcquireDynamic(limit, used, weight int) (ok bool) {
	return used <= limit && weight > 0 && weight <= limit-used
}
