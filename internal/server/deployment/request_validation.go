package deployment

import (
	"strconv"
	"unicode"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// MaxNameRunes bounds a name an operator gives a resource.
const MaxNameRunes = 256

// TextFieldError refuses a text field that is not valid text, holds a
// control character or is longer than maximum characters; nil accepts it.
func TextFieldError(field, value string, maximum int) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum || containsControl(value) {
		detail := "must contain valid text without control characters and at most " + strconv.Itoa(maximum) + " characters"
		return productError(messages.ErrorFieldInvalid, field+" "+detail, map[string]any{"field": field, "detail": detail})
	}
	return nil
}

// validateNames checks a deployment's name and the name it serves under.
func validateNames(name, servedName string) error {
	if err := TextFieldError("name", name, MaxNameRunes); err != nil {
		return err
	}
	return TextFieldError("served_name", servedName, MaxNameRunes)
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
