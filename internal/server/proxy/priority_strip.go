package proxy

import "net/http"

// priorityField is the request field vLLM and SGLang order their queues by,
// and priorityHeader the header vLLM 0.31 lets override it. Only the gateway
// may set a request's priority: a client's value would let any key jump the
// queue of an engine running priority scheduling.
const (
	priorityField  = "priority"
	priorityHeader = "X-Vllm-Priority"
)

// bodyNames reports, in one pass over a request body the gateway owns fields
// in (ownedJSONBody), which of keys it mentions as a quoted string anywhere
// (a superset of its top-level fields). A body that cannot be replayed
// mentions none.
func bodyNames(r *http.Request, keys ...string) ([]bool, error) {
	found := make([]bool, len(keys))
	if r.GetBody == nil {
		return found, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return found, err
	}
	defer body.Close()
	needles := make([][]byte, len(keys))
	for i, key := range keys {
		needles[i] = []byte(`"` + key + `"`)
	}
	err = readerContainsEach(body, needles, found)
	return found, err
}
