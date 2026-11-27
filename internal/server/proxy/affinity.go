package proxy

import (
	"bufio"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func affinityKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-GPUWorks-Affinity-Key")); key != "" {
		r.Header.Del("X-GPUWorks-Affinity-Key")
		return key
	}
	if r.GetBody == nil {
		return ""
	}
	body, err := r.GetBody()
	if err != nil {
		return ""
	}
	defer body.Close()
	return affinityKeyJSON(body)
}

func affinityKeyJSON(source io.Reader) string {
	dec := json.NewDecoder(bufio.NewReader(source))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ""
	}
	var fallback string
	for dec.More() {
		keyToken, err := dec.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return ""
		}
		switch key {
		case "messages":
			if value, ok := affinityMessages(dec); ok {
				return value
			}
		case "prompt", "input":
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return ""
			}
			if len(raw) > 4<<10 {
				raw = raw[:4<<10]
			}
			if fallback == "" {
				fallback = string(raw)
			}
		default:
			if skipJSONValue(dec) != nil {
				return ""
			}
		}
	}
	return fallback
}

func affinityMessages(dec *json.Decoder) (string, bool) {
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return "", false
	}
	var out []byte
	for dec.More() {
		var item json.RawMessage
		if err := dec.Decode(&item); err != nil {
			return "", false
		}
		out = append(out, item...)
		var role struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(item, &role)
		if strings.EqualFold(role.Role, "user") || len(out) >= 64<<10 {
			if len(out) > 64<<10 {
				out = out[:64<<10]
			}
			return string(out), true
		}
	}
	return string(out), true
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	for dec.More() {
		if delim == '{' {
			if _, err := dec.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func fnv64(data []byte) uint64 { h := fnv.New64a(); _, _ = h.Write(data); return h.Sum64() }
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// pickAffinity leases the candidate key ranks first that is not cooling down
// or loaded well above the average, else the least loaded; for adm only
// among those with a free run slot, busy as for pick. The average counts
// every eligible candidate, so a full one still weighs on it.
func (b *balancer) pickAffinity(candidates []candidate, key string, excluded map[string]bool, online func(string) bool, adm admission) (l *lease, busy bool) {
	if strings.TrimSpace(key) == "" {
		return b.pick(candidates, excluded, online, adm)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	nowFn := b.now
	if nowFn == nil {
		nowFn = time.Now
	}
	type scored struct {
		c     candidate
		score uint64
	}
	items := make([]scored, 0, len(candidates))
	total, eligible := 0, 0
	for _, c := range candidates {
		if excluded[c.instanceUUID] || (online != nil && !online(c.nodeUUID)) {
			continue
		}
		s := b.state(c.instanceUUID)
		eligible++
		if s.cooldownUntil.IsZero() || nowFn().After(s.cooldownUntil) {
			total += s.inflight.total
		}
		if !b.hasRoom(c, adm) {
			busy = true
			continue
		}
		uuidHash := c.uuidHash
		if uuidHash == 0 {
			uuidHash = fnv64([]byte(c.instanceUUID))
		}
		items = append(items, scored{c, mix64(fnv64([]byte(key)) ^ uuidHash)})
	}
	if len(items) == 0 {
		return nil, busy
	}
	sort.Slice(items, func(i, j int) bool { return items[i].score > items[j].score })
	avg := float64(total+1) / float64(eligible)
	limit := int(avg * 1.5)
	if limit < 4 {
		limit = 4
	}
	for _, item := range items {
		s := b.state(item.c.instanceUUID)
		if (s.cooldownUntil.IsZero() || nowFn().After(s.cooldownUntil)) && s.inflight.total < limit {
			return b.take(item.c, adm), false
		}
	}
	best := items[0]
	for _, item := range items[1:] {
		if b.state(item.c.instanceUUID).inflight.total < b.state(best.c.instanceUUID).inflight.total {
			best = item
		}
	}
	return b.take(best.c, adm), false
}

func (p *Proxy) affinityKey(modelName string, r *http.Request) string {
	// The explicit header is cheap to inspect and must always be stripped, even
	// when the selected deployment does not use prefix affinity.
	if strings.TrimSpace(r.Header.Get("X-GPUWorks-Affinity-Key")) != "" {
		return affinityKey(r)
	}
	if p.routes == nil || !p.routes.needsAffinity(modelName) {
		return ""
	}
	return affinityKey(r)
}
