package workflow

// ProducesChanges includes scoped document writers: granting writes is an
// effect, irrespective of the older change_producer spelling. Artifact-only
// workflows still use this for isolation, while their code floors stay exempt.
func ProducesChanges(d Document, s Stage) bool {
	if s.ChangeProducer || s.Generate != nil || len(s.Writes) > 0 {
		return true
	}
	for _, name := range stageRoles(s) {
		if len(d.Roles[name].Writes) > 0 {
			return true
		}
	}
	return false
}
