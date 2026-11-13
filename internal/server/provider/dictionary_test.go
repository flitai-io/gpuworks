package provider

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/localization"
)

func providerTextComplete(text localization.Text) bool {
	for _, language := range requiredProviderLocales {
		if text[language] == "" {
			return false
		}
	}
	return true
}

func TestProviderDictionaryCoversKindsCredentialsAndConnections(t *testing.T) {
	for _, kind := range knownProviderKinds {
		if !providerTextComplete(labelFor(kind)) {
			t.Errorf("provider kind %q has incomplete labels: %#v", kind, labelFor(kind))
		}
		if category := CategoryOf(kind); category != CategoryPublicCloud && category != CategoryPrivateCloud {
			t.Errorf("provider kind %q category = %q, want a declared category", kind, category)
		}
		if !strings.HasPrefix(kindHomepages[kind], "https://") {
			t.Errorf("provider kind %q homepage = %q, want an https URL", kind, kindHomepages[kind])
		}
		for _, field := range credentialFieldsFor(kind) {
			if !providerTextComplete(field.Label) || !providerTextComplete(field.Placeholder) {
				t.Errorf("provider %q credential %q is incomplete: %#v", kind, field.Name, field)
			}
		}
	}
	for _, connectionType := range knownSSHConnectionTypes {
		if !providerTextComplete(SSHConnectionLabel(connectionType)) {
			t.Errorf("SSH connection %q has incomplete labels: %#v", connectionType, SSHConnectionLabel(connectionType))
		}
	}
}

func TestUnknownProviderDictionaryValuesRemainStable(t *testing.T) {
	if got := labelFor("extension-provider"); got[localization.English] != "extension-provider" || got[localization.Chinese] != "extension-provider" {
		t.Fatalf("unknown provider label = %#v", got)
	}
	if got := SSHConnectionLabel("tunnel"); got[localization.English] != "tunnel" || got[localization.Chinese] != "tunnel" {
		t.Fatalf("unknown SSH connection label = %#v", got)
	}
}
