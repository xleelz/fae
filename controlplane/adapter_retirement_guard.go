package controlplane

// @ ensures result == (!retired)
func AdapterAcceptsBinding(retired bool) (result bool) { return !retired }
