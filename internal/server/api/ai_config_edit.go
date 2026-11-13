package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// aiConfigTestTimeout bounds a connection test: a wrong address must be
// reported while the operator still watches the dialog, not after the
// assistant's full call timeout.
const aiConfigTestTimeout = 10 * time.Second

// updateAIConfig changes one AI service in place (PATCH /api/v1/ai/configs/{id}).
// Partial by design: the edit dialog holds only the fields it shows, and the
// key it never received must survive an edit of the name.
func (a *apiHandlers) updateAIConfig(w http.ResponseWriter, r *http.Request) {
	var req dto.AIConfigPatch
	if !decodeJSONBody(w, r, &req) {
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	patch := store.AIConfigPatch{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeFieldInvalid(w, "name", "is required")
			return
		}
		// The stored name is read first so an unchanged one skips the bound; a
		// missing row answers "" here and gets its own 404 from the write below,
		// which is the failure the caller needs to see.
		if !validateAIConfigName(w, name, a.aiConfigStoredName(id)) {
			return
		}
		patch.Name = &name
	}
	if req.Preset != nil {
		preset := strings.TrimSpace(*req.Preset)
		if !validateAIColumn(w, "preset", preset, 32) {
			return
		}
		patch.Preset = &preset
	}
	if req.BaseURL != nil {
		base, ok := validateAIBaseURL(w, *req.BaseURL, true)
		if !ok {
			return
		}
		patch.BaseURL = &base
	}
	if req.Model != nil {
		model := strings.TrimSpace(*req.Model)
		if model == "" {
			writeFieldInvalid(w, "model", "is required")
			return
		}
		if !validateAIColumn(w, "model", model, 64) {
			return
		}
		patch.Model = &model
	}
	if req.APIKey != nil {
		// An empty key keeps whatever is stored, a legacy plaintext value
		// included: the dialog never received the key and so cannot send it
		// back.
		if key := strings.TrimSpace(*req.APIKey); key != "" {
			encrypted, err := store.EncryptAIKey(a.d.AISecretbox, key)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			patch.APIKey = &encrypted
		}
	}
	if patch.Preset != nil {
		// A preset names a provider, so it brings that provider's address and
		// model along unless this patch names its own - the same fill a create
		// applies.
		patch.BaseURL, patch.Model = fillAIConfigPreset(*patch.Preset, patch.BaseURL, patch.Model)
	}
	patch.IsDefault = req.IsDefault
	c, err := a.d.Store.UpdateAIConfig(parseUint(chi.URLParam(r, "id")), patch)
	if err != nil {
		writeAIStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, aiConfigView(c))
}

// fillAIConfigPreset answers the address and model a patch should store for a
// named preset, the way a create fills them from the same table.
//
// Without this, a client that patched only {"preset": "bailian"} got a row
// labelled Alibaba Cloud that still called api.deepseek.com with a DeepSeek
// model - the label and the traffic disagreeing, which is the worst kind of
// wrong for a setting whose purpose is to say where data goes. The console hid
// it only because the dialog fills the form client-side when the preset changes;
// an API contract cannot depend on the one client that remembers to.
//
// A patch that names base_url or model keeps the value it named: the preset
// fills gaps and never overwrites an explicit value.
func fillAIConfigPreset(preset string, baseURL, model *string) (*string, *string) {
	p, ok := aihelper.Presets[preset]
	if !ok {
		return baseURL, model
	}
	if baseURL == nil && p.BaseURL != "" {
		base, _ := aihelper.NormalizeBaseURL(p.BaseURL)
		baseURL = &base
	}
	if model == nil && p.Model != "" {
		filled := p.Model
		model = &filled
	}
	return baseURL, model
}

// testAIConfig asks an AI service whether it answers
// (POST /api/v1/ai/configs/test) with the values a form holds, before they are
// saved. It makes the call the assistant makes, through the same outbound
// policy, so its answer is the assistant's answer. Nothing is stored.
func (a *apiHandlers) testAIConfig(w http.ResponseWriter, r *http.Request) {
	var req dto.AIConfigTest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	base, ok := validateAIBaseURL(w, req.BaseURL, true)
	if !ok {
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeFieldInvalid(w, "model", "is required")
		return
	}
	if !validateAIColumn(w, "model", model, 64) {
		return
	}
	key, ok := a.aiConfigTestKey(w, req, base)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), aiConfigTestTimeout)
	defer cancel()
	result := aihelper.NewClientWithPrivateHosts(
		aihelper.Endpoint{BaseURL: base, APIKey: key, Model: model},
		a.outboundPrivateHosts,
	).Probe(ctx)
	writeJSON(w, http.StatusOK, dto.AIConfigTestResult{
		Outcome: string(result.Outcome), Status: result.Status, Detail: result.Detail,
		BaseURL: base, LatencyMS: result.LatencyMS,
	})
}

// aiConfigTestKey picks the key a test uses. The edit dialog leaves the key
// blank to mean "keep the stored one", and the stored one is used only while the
// address being tested is still the stored address: a test must not be a way to
// send a stored secret to an address of the caller's choosing.
//
// Both sides are normalized before they are compared, through the same
// aihelper.NormalizeBaseURL the store wrote the request base with. Comparing the
// normalized request base with the RAW stored value answered "a different
// address" for every row written before addresses were normalized on write - a
// stored "http://host:8800/v1/chat/completions" never equals the ".../v1" the
// dialog sends - so the probe ran keyless, the endpoint answered 401, and the
// dialog told the operator their key was rejected and to check it. These are
// exactly the rows the ticket was filed about, and the advice sent the operator
// to fix a key that was correct.
//
// Normalizing does not weaken the guard: normalization strips a call path from
// the PATH, and a query string is kept. The host is compared unchanged either
// way, so a key still cannot be sent anywhere but the stored service's own host
// (and, with the query kept, its own tenant: an Azure-style base carries
// ?api-version=... as part of the address, and a test that dropped it would call
// a different address than the assistant does).
func (a *apiHandlers) aiConfigTestKey(w http.ResponseWriter, req dto.AIConfigTest, base string) (string, bool) {
	if key := strings.TrimSpace(req.APIKey); key != "" || req.ID == 0 {
		return key, true
	}
	stored, err := a.d.Store.AIConfigByID(req.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", true
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	if storedBase, _ := aihelper.NormalizeBaseURL(stored.BaseURL); storedBase != base {
		return "", true
	}
	key, err := store.DecryptAIKey(a.d.AISecretbox, stored.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return "", false
	}
	return key, true
}

// aiConfigView is the public form of a stored service: what the console shows,
// and never the key.
func aiConfigView(c *store.AIConfig) dto.AIConfigView {
	return dto.AIConfigView{ID: c.ID, Name: c.Name, Preset: c.Preset, BaseURL: c.BaseURL, Model: c.Model, IsDefault: c.IsDefault}
}

// aiConfigStoredName answers the name a stored service holds, or "" when the id
// names no row.
//
// A row that cannot be read answers "": the bound is then applied to the value
// as if it were new, and the write that follows reports the real reason the
// caller has to act on (404 for a missing service, 500 for a storage failure).
func (a *apiHandlers) aiConfigStoredName(id uint) string {
	stored, err := a.d.Store.AIConfigByID(id)
	if err != nil {
		return ""
	}
	return stored.Name
}

// writeAIStoreErr answers a config write: a missing service is a 404, a patch
// that would leave the stored services with no default is a 409 - the request
// is well formed and the row exists, the state it asks for is what conflicts -
// and anything else a storage failure.
func writeAIStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, store.ErrAIDefaultRequired) {
		writeProductError(w, http.StatusConflict, ErrorAIDefaultRequired,
			"this is the AI service the assistant uses; make another service the default first, or delete this one",
			map[string]any{"field": "is_default", "detail": "the service the assistant uses cannot stop being the default"})
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// writeFieldInvalid rejects the value of one field, named so the console can
// mark the input instead of showing a banner.
func writeFieldInvalid(w http.ResponseWriter, field, detail string) {
	writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, field+" "+detail, map[string]any{"field": field, "detail": detail})
}

// aiConfigNameMaxRunes bounds the display name of an AI service, in characters,
// on create and on edit alike.
const aiConfigNameMaxRunes = 64

// validateAIConfigName is the one bound both a create and an edit apply to a
// service name, so that a name the API accepts is a name the API can later
// change. When the two disagreed - create accepted up to deployment.MaxNameRunes (256) and
// edit held 64 - the API could store a 70-character name and then refuse every
// edit of that service, because the edit dialog always sends the name back and
// the edit path re-validated it: the row was permanently uneditable through the
// console, which is how the defect was filed.
//
// stored is the name the row already holds, and is "" on a create. A patch that
// sends the stored name back unchanged skips the bound: rows created by a build
// with the wider limit are already in databases in the field, and refusing their
// name would leave the operator unable to fix anything else about that service
// without inventing a new name for it first. A name the operator is changing now
// is held to the bound, because that is a value they are writing and can see the
// rule for.
func validateAIConfigName(w http.ResponseWriter, value, stored string) bool {
	if !utf8.ValidString(value) || containsControl(value) {
		writeFieldInvalid(w, "name", "must contain valid text without control characters")
		return false
	}
	if value == stored {
		return true
	}
	if utf8.RuneCountInString(value) > aiConfigNameMaxRunes {
		writeFieldInvalid(w, "name", "must be at most "+strconv.Itoa(aiConfigNameMaxRunes)+" characters")
		return false
	}
	return true
}

// validateAIColumn bounds one stored text field of an AI service by its
// character limit.
//
// The count is characters, and the message says so: the same function once
// compared len(value), which is bytes, while telling the operator the limit was
// in characters. Those differ for every name written in Chinese, and the
// disagreement was invisible until a value was long enough that the byte count
// crossed a limit the character count had not reached. The SQLite store keeps
// these columns as TEXT, which has no enforced width, so the character count is
// also the bound the schema can honestly claim; where a column width does matter
// it is the store's to enforce on write, not a message to mis-state here.
func validateAIColumn(w http.ResponseWriter, field, value string, maximum int) bool {
	if !utf8.ValidString(value) || containsControl(value) {
		writeFieldInvalid(w, field, "must contain valid text without control characters")
		return false
	}
	if utf8.RuneCountInString(value) > maximum {
		writeFieldInvalid(w, field, "must be at most "+strconv.Itoa(maximum)+" characters")
		return false
	}
	return true
}

// validateAIBaseURL normalizes an endpoint address and rejects one the client
// could not call. The platform stores base addresses, so a pasted call path is
// dropped (aihelper.NormalizeBaseURL); what remains must be an absolute
// http(s) address with a host.
//
// required is true where a missing address is the caller's error: an edit and
// a connection test both name the address they mean. A create may omit it - a
// preset fills it in, and a service stored without one is only a configuration
// the assistant reports as unusable.
func validateAIBaseURL(w http.ResponseWriter, raw string, required bool) (string, bool) {
	base, _ := aihelper.NormalizeBaseURL(raw)
	if base == "" {
		if !required {
			return "", true
		}
		writeFieldInvalid(w, "base_url", "is required")
		return "", false
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeFieldInvalid(w, "base_url", "must be an http or https address such as https://api.deepseek.com/v1")
		return "", false
	}
	if !validateAIColumn(w, "base_url", base, 255) {
		return "", false
	}
	return base, true
}
