package provider

import (
	"embed"
	"fmt"

	"gpuworks.ai/gpuworks/pkg/localization"
)

var requiredProviderLocales = []string{localization.English, localization.Chinese}

//go:embed locales/*.json
var providerLocaleFiles embed.FS

type credentialFieldLocale struct {
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
}

type providerLocale struct {
	ProviderKinds    map[string]string                `json:"provider_kinds"`
	CredentialFields map[string]credentialFieldLocale `json:"credential_fields"`
	SSHConnections   map[string]string                `json:"ssh_connections"`
}

var knownProviderKinds = []string{"aliyun", "autodl", "compshare", "digitalocean", "runpod", "vast", "zstack"}

// kindHomepages is each known vendor's public site, linked from the provider
// type cards.
var kindHomepages = map[string]string{
	"aliyun":       "https://www.aliyun.com",
	"autodl":       "https://www.autodl.com",
	"compshare":    "https://www.compshare.cn",
	"digitalocean": "https://www.digitalocean.com",
	"runpod":       "https://www.runpod.io",
	"vast":         "https://vast.ai",
	"zstack":       "https://www.zstack.io",
}

// kindCategories is the platform each known kind connects: ZStack Cloud is
// deployed on the customer's premises, the rental vendors are public clouds.
var kindCategories = map[string]Category{
	"aliyun":       CategoryPublicCloud,
	"autodl":       CategoryPublicCloud,
	"compshare":    CategoryPublicCloud,
	"digitalocean": CategoryPublicCloud,
	"runpod":       CategoryPublicCloud,
	"vast":         CategoryPublicCloud,
	"zstack":       CategoryPrivateCloud,
}

var knownCredentialFields = []string{"access_key_id", "access_key_secret", "api_key", "endpoint", "private_key", "public_key", "regions", "role_arn", "ssh_password"}
var knownSSHConnectionTypes = []string{"cloud_assistant", "direct", "proxy"}

var providerLocales map[string]providerLocale

func init() {
	locales, err := localization.LoadJSONFiles[providerLocale](providerLocaleFiles, "locales/%s.json", requiredProviderLocales)
	if err == nil {
		err = validateProviderLocales(locales)
	}
	if err != nil {
		panic(fmt.Sprintf("load provider locales: %v", err))
	}
	providerLocales = locales
}

func validateProviderLocales(locales map[string]providerLocale) error {
	for _, language := range requiredProviderLocales {
		locale, ok := locales[language]
		if !ok {
			return fmt.Errorf("missing required locale %q", language)
		}
		if err := validateProviderLabels(language, "provider kind", knownProviderKinds, locale.ProviderKinds); err != nil {
			return err
		}
		credentialLabels := make(map[string]string, len(locale.CredentialFields))
		for key, entry := range locale.CredentialFields {
			credentialLabels[key] = entry.Label
			if key != "endpoint" && entry.Placeholder == "" {
				return fmt.Errorf("locale %s credential field %q has no placeholder", language, key)
			}
		}
		if err := validateProviderLabels(language, "credential field", knownCredentialFields, credentialLabels); err != nil {
			return err
		}
		if err := validateProviderLabels(language, "SSH connection", knownSSHConnectionTypes, locale.SSHConnections); err != nil {
			return err
		}
	}
	return nil
}

func validateProviderLabels(language, kind string, keys []string, labels map[string]string) error {
	expected := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		expected[key] = struct{}{}
	}
	for key := range labels {
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("locale %s contains unknown %s %q", language, kind, key)
		}
	}
	for key := range expected {
		if labels[key] == "" {
			return fmt.Errorf("locale %s is missing %s %q", language, kind, key)
		}
	}
	return nil
}

func labelFor(kind string) localization.Text {
	if _, known := providerLocales[localization.English].ProviderKinds[kind]; !known {
		return localization.SameText(kind, requiredProviderLocales...)
	}
	label := make(localization.Text, len(requiredProviderLocales))
	for _, language := range requiredProviderLocales {
		label[language] = providerLocales[language].ProviderKinds[kind]
	}
	return label
}

func credentialField(textKey string, field CredentialField) CredentialField {
	localizeCredentialField(&field, textKey)
	return field
}

func localizeCredentialField(field *CredentialField, textKey string) {
	field.Label = make(localization.Text, len(requiredProviderLocales))
	if field.Placeholder == nil && providerLocales[localization.English].CredentialFields[textKey].Placeholder != "" {
		field.Placeholder = make(localization.Text, len(requiredProviderLocales))
	}
	for _, language := range requiredProviderLocales {
		entry := providerLocales[language].CredentialFields[textKey]
		field.Label[language] = entry.Label
		if field.Placeholder != nil && entry.Placeholder != "" {
			field.Placeholder[language] = entry.Placeholder
		}
	}
}

// SSHConnectionLabel returns server-owned display text without changing the
// stable direct/proxy connection type sent to automation clients.
func SSHConnectionLabel(connectionType string) localization.Text {
	if _, known := providerLocales[localization.English].SSHConnections[connectionType]; !known {
		return localization.SameText(connectionType, requiredProviderLocales...)
	}
	label := make(localization.Text, len(requiredProviderLocales))
	for _, language := range requiredProviderLocales {
		label[language] = providerLocales[language].SSHConnections[connectionType]
	}
	return label
}
