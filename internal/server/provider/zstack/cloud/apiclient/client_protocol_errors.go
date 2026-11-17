package apiclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type ProtocolFailure string

const (
	APIRouteMissing       ProtocolFailure = "api_route_missing"
	ResourceConfigUnbound ProtocolFailure = "resource_config_unbound"
)

// External text: RestServer's unmapped-route response and ResourceConfigApiInterceptor's
// cannot-bind diagnostic in Cloud 4.8.0. Match the requested path/configuration,
// retain only a reason, and never expose the response text through APIError.
func protocolFailure(method, path string, reply cloudReply, apiErr *APIError) ProtocolFailure {
	if apiErr.Kind == ErrorAuthentication {
		return ""
	}
	uri, err := url.Parse("/v1" + path)
	if err != nil {
		return ""
	}
	if reply.status == http.StatusNotFound && string(reply.body) == "no api mapping to "+uri.Path {
		return APIRouteMissing
	}
	if apiErr.Code != "SYS.1007" || (reply.status != http.StatusServiceUnavailable && reply.status != http.StatusBadRequest) {
		return ""
	}
	parts := strings.Split(strings.Trim(uri.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "v1" || parts[1] != "resource-configurations" {
		return ""
	}
	var category, name string
	switch {
	case method == http.MethodGet && len(parts) == 5:
		category, name = parts[3], parts[4]
	case method == http.MethodPut && len(parts) == 6 && parts[5] == "actions":
		category, name = parts[2], parts[3]
	default:
		return ""
	}
	var detail struct {
		Details string `json:"details"`
	}
	if json.Unmarshal(reply.Error, &detail) != nil {
		return ""
	}
	switch detail.Details {
	case fmt.Sprintf("global config[category:%s, name:%s.%s] cannot bind resource", category, category, name),
		fmt.Sprintf("全局配置[类别：%s，名称：%s.%s]无法绑定资源", category, category, name):
		return ResourceConfigUnbound
	}
	return ""
}
