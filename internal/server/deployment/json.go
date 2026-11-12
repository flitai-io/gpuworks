package deployment

import (
	"encoding/json"
)

func jsonStr(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
