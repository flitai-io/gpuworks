package zstackcloud_test

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type cloudGPUAPIOptions struct {
	pciOnly         bool
	allocationField bool
	hostCPU         bool
}

type cloudGPUAPIState struct {
	disabled                           bool
	description                        string
	disables, enables                  int
	nativeLists, nativeTargets         int
	pciLists, pciTargets, missingRoute int
	globalCPUReads                     int
}

type cloudGPUAPI struct {
	url      string
	mu       sync.Mutex
	observed cloudGPUAPIState
}

func (c *cloudGPUAPI) state() cloudGPUAPIState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observed
}

func (c *cloudGPUAPI) appendDescription(note string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed.description += note
}

// Models Cloud HTTP responses and device state; tests select the available API capabilities.
func newCloudGPUAPI(t *testing.T, options cloudGPUAPIOptions, containerRemoved func() bool) *cloudGPUAPI {
	t.Helper()
	c := &cloudGPUAPI{observed: cloudGPUAPIState{description: "原有备注"}}
	common := cloudInventoryFixture(t)
	var pendingJob, pendingState, pendingDescription string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validCloudAccessKey(r) {
			t.Error("Cloud received an invalid AccessKey date or signature")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/hosts" && r.URL.Query().Has("q") && r.URL.Query().Get("q") != "hypervisorType=KVM":
			if query := r.URL.Query(); len(query) != 1 || len(query["q"]) != 1 || query.Get("q") != "uuid=host-a" {
				t.Errorf("unexpected selected host query: %s", r.URL)
				http.Error(w, "invalid host query", http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"inventories":[{"uuid":"host-a","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`)
		case r.Method == http.MethodGet && (r.URL.Path == "/v1/gpu-device/gpu-devices" || r.URL.Path == "/v1/pci-device/pci-devices"):
			pci := r.URL.Path == "/v1/pci-device/pci-devices"
			if options.pciOnly && !pci {
				c.observed.missingRoute++
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "no api mapping to "+r.URL.Path)
				return
			}
			if !options.pciOnly && pci {
				t.Error("an available GPU API must not fall back to PCI queries")
				http.Error(w, "unexpected PCI query", http.StatusBadRequest)
				return
			}
			query := r.URL.Query()
			targeted := strings.HasPrefix(query.Get("q"), "uuid=")
			if targeted {
				if len(query) != 1 || len(query["q"]) != 1 || query.Get("q") != "uuid=gpu-a" {
					t.Errorf("unexpected selected GPU query: %s", r.URL)
					http.Error(w, "invalid GPU query", http.StatusBadRequest)
					return
				}
			} else {
				wantQuerySize := 3
				if pci {
					wantQuerySize++
					if len(query["q"]) != 1 || query.Get("q") != "type?=GPU_Video_Controller,GPU_3D_Controller,GPU_Processing_Accelerators,GPU_Co_Processor" {
						t.Errorf("unexpected PCI GPU type filter: %s", r.URL)
						http.Error(w, "invalid filter", http.StatusBadRequest)
						return
					}
				}
				if len(query) != wantQuerySize || query.Get("start") != "0" || query.Get("limit") != "100" || query.Get("sort") != "uuid" {
					t.Errorf("unexpected GPU pagination: %s", r.URL)
					http.Error(w, "invalid pagination", http.StatusBadRequest)
					return
				}
			}
			switch {
			case pci && targeted:
				c.observed.pciTargets++
			case pci:
				c.observed.pciLists++
			case targeted:
				c.observed.nativeTargets++
			default:
				c.observed.nativeLists++
			}
			state, freeAllocation, usedAllocation := "Enabled", "", ""
			if c.observed.disabled {
				state = "Disabled"
			}
			if options.allocationField {
				freeAllocation, usedAllocation = `,"allocateStatus":"Unallocated"`, `,"allocateStatus":"Allocated"`
			}
			free := fmt.Sprintf(`{"uuid":"gpu-a","hostUuid":"host-a","name":"NVIDIA Test","type":"GPU_3D_Controller","pciDeviceAddress":"0000:01:00.0","state":%q,"status":"Active","virtStatus":"UNVIRTUALIZABLE","description":%q%s}`, state, c.observed.description, freeAllocation)
			devices := []string{free}
			if !targeted {
				devices = append(devices, `{"uuid":"gpu-used","hostUuid":"host-a","name":"NVIDIA Test","type":"GPU_3D_Controller","pciDeviceAddress":"0000:02:00.0","state":"Enabled","status":"Attached","virtStatus":"UNVIRTUALIZABLE","vmInstanceUuid":"vm-1"`+usedAllocation+`}`)
			}
			fmt.Fprintf(w, `{"inventories":[%s]}`, strings.Join(devices, ","))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/resource-configurations/") && strings.HasSuffix(r.URL.Path, "/kvm/reservedCpu") && !options.hostCPU:
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"code":"SYS.1007","details":"全局配置[类别：kvm，名称：kvm.reservedCpu]无法绑定资源"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/global-configurations":
			q := r.URL.Query()
			if options.hostCPU || len(q) != 1 || len(q["q"]) != 2 || q["q"][0] != "category=kvm" || q["q"][1] != "name=reservedCpu" {
				t.Errorf("unexpected global CPU query: %s", r.URL)
				http.Error(w, "invalid global config query", http.StatusBadRequest)
				return
			}
			c.observed.globalCPUReads++
			io.WriteString(w, `{"inventories":[{"category":"kvm","name":"reservedCpu","value":"3"}]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/pci-device/pci-devices/gpu-a/actions":
			mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				t.Error("Cloud mutations must use JSON")
				http.Error(w, "invalid content type", http.StatusBadRequest)
				return
			}
			var body io.Reader = r.Body
			if options.pciOnly && !strings.EqualFold(params["charset"], "utf-8") {
				// Cloud 4.8's Servlet reader defaults to ISO-8859-1 without a charset.
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read Cloud request body: %v", err)
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				decoded := make([]rune, len(raw))
				for i, value := range raw {
					decoded[i] = rune(value)
				}
				body = strings.NewReader(string(decoded))
			}
			var action struct {
				Update struct {
					State       string `json:"state"`
					Description string `json:"description"`
				} `json:"updatePciDevice"`
			}
			decoder := json.NewDecoder(body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&action); err != nil || (action.Update.State != "Disabled" && action.Update.State != "Enabled") {
				t.Errorf("invalid GPU mutation: %+v, %v", action, err)
				http.Error(w, "invalid mutation", http.StatusBadRequest)
				return
			}
			if action.Update.State == "Enabled" && !containerRemoved() {
				t.Error("Cloud GPU released before the agent removed its container")
				http.Error(w, "container still exists", http.StatusConflict)
				return
			}
			pendingState, pendingDescription = action.Update.State, action.Update.Description
			if pendingState == "Disabled" {
				c.observed.disables++
			} else {
				c.observed.enables++
			}
			pendingJob = fmt.Sprintf("/v1/api-jobs/protocol-gpu-%d", c.observed.disables+c.observed.enables)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, `{"location":%q}`, pendingJob)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/api-jobs/"):
			if pendingState == "" || r.URL.Path != pendingJob {
				t.Errorf("Cloud queried the wrong mutation job: %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			c.observed.disabled = pendingState == "Disabled"
			c.observed.description = pendingDescription
			io.WriteString(w, `{}`)
		default:
			// Common host discovery and SSH checks; all other mutations are rejected.
			common.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c.url = server.URL
	return c
}
