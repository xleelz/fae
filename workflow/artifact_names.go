package workflow

// DeclaresNamedArtifact includes public signature names whose producer may be
// a child that has not finished yet. A request records this declared name.
func DeclaresNamedArtifact(d Document, name string) bool {
	if _, ok := ArtifactDeclaration(d, name); ok {
		return true
	}
	if d.Signature != nil {
		if _, ok := d.Signature.Inputs[name]; ok {
			return true
		}
		if _, ok := d.Signature.Outputs[name]; ok {
			return true
		}
	}
	return false
}
