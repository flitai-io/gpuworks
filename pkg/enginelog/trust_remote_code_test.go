package enginelog_test

import (
	"os"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/pkg/enginelog"
)

func TestClassifyRequiresRemoteCodeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
		want bool
	}{
		{"enabled argument", "INFO non-default args: {'trust_remote_code': True}", false},
		{"disabled argument", "INFO server_args={'trust_remote_code': False}", false},
		{"ignored argument", "WARNING The argument `trust_remote_code` is to be used with Auto classes. It has no effect here and is ignored.", false},
		{"traceback source", "    trust_remote_code = resolve_trust_remote_code(", false},
		{"unrelated failure", "INFO non-default args: {'trust_remote_code': True}\nModuleNotFoundError: No module named 'missing_backend'", false},
		{"configuration refusal", "ValueError: Loading /models/x requires you to execute the configuration file in that repo on your local machine.", true},
		{"explicit permission request", "Please pass the argument `trust_remote_code=True` to allow custom code to be run.", true},
		{"explicit configuration request", "ValueError: Please set `trust_remote_code=True` to load this model.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := enginelog.Classify(tc.log)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("normal configuration or unrelated error became a diagnosis: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Code != enginelog.CodeTrustRemoteCodeRequired || !reflect.DeepEqual(got[0].Lines(), []string{tc.log}) {
				t.Fatalf("permission refusal lost its code or evidence: %+v", got)
			}
		})
	}
}

func TestClassifySGLangConfigurationIsNotAFailure(t *testing.T) {
	// Reuse the captured engine output rather than inventing a reduced args dump.
	data, err := os.ReadFile("testdata/sglang-nextn-tp4.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := enginelog.Classify(string(data)); len(got) != 0 {
		t.Fatalf("configuration output became a %q diagnosis", got[0].Code)
	}
}
