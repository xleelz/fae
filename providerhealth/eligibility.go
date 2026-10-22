package providerhealth

// Eligible is the production selection predicate; scores are converted to
// thousandths before calling it so the executable contract uses exact integers.
// @ ensures allowed == (score >= 50000 && score <= 100000)
// @ ensures allowed ==> score >= 50000
func Eligible(score int) (allowed bool) { return score >= 50000 && score <= 100000 }
