package gpu

// limitedDiagnostic keeps the last 400 stderr bytes without letting a noisy
// CLI allocate unbounded memory. Successful structured stdout stays untouched.
type limitedDiagnostic struct{ data []byte }

func (b *limitedDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if n >= 400 {
		b.data = append(b.data[:0], p[n-400:]...)
		return n, nil
	}
	if len(b.data)+n > 400 {
		b.data = b.data[len(b.data)+n-400:]
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (b *limitedDiagnostic) String() string { return string(b.data) }
