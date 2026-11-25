package app

import (
	"os"
	"strings"
	"testing"
)

// The documentation lists every cause the journal can name, in both
// languages, next to the log line that carries it.
func TestAdmissionCloseCausesAreDocumented(t *testing.T) {
	for _, page := range []string{"../../../docs/explanation/control-plane-cluster.md", "../../../docs-en/explanation/control-plane-cluster.md"} {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.Contains(text, "`closed business admission`") || !strings.Contains(text, "`caller`") {
			t.Fatalf("%s names neither the log line nor its field", page)
		}
		for cause := range admissionCloseCauses {
			if !strings.Contains(text, "`"+string(cause)+"`") {
				t.Errorf("%s does not list the close cause %s", page, cause)
			}
		}
	}
}

// The troubleshooting pages name a refusal code and log strings for a revoked
// member; they are the ones the server gives.
func TestRevokedMemberTroubleshootingNamesWhatTheServerGives(t *testing.T) {
	for _, page := range []string{"../../../docs/how-to/troubleshooting.md", "../../../docs-en/how-to/troubleshooting.md"} {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{
			"`" + refusalMemberRevoked + "`", "`" + refusalIdentityConflict + "`", "`" + refusalProofMissing + "`", "`" + refusalProofExpired + "`",
			businessRefusalHeader,
			"closed business admission caller=" + string(closeProofFailed),
			"candidate material proof failed while business admission is closed",
			"err_class=admission_rejected", "err_class=identity_conflict", "`failure_stage`", "caller=" + string(closeProofExpired),
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not name %s", page, want)
			}
		}
	}
}
