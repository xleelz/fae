package scheduler

import "errors"

var ErrRecoveryUnproven = errors.New("lease recovery proof incomplete or mismatched")

// RecoverProof removes only the exact request linked by a terminal journal proof.
// Missing requests are idempotent after a crash between the two durable stores.
func (p *Pool) RecoverProof(proof OperationProof, ownerDead bool) (bool, error) {
	for i, h := range p.Holders {
		if h.ID == proof.ID {
			if !CanRecoverLease(ownerDead, h.Owner == proof.Owner, proof.State == "reserved", proof.State == "retired") {
				return false, ErrRecoveryUnproven
			}
			p.Holders = append(p.Holders[:i], p.Holders[i+1:]...)
			return true, nil
		}
	}
	for i, w := range p.Waiting {
		if w.ID == proof.ID {
			if !CanRecoverLease(ownerDead, w.Owner == proof.Owner, proof.State == "reserved", proof.State == "retired") {
				return false, ErrRecoveryUnproven
			}
			p.Waiting = append(p.Waiting[:i], p.Waiting[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}
