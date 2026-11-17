package proxy

import (
	"encoding/json"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// modalityMaxAge bounds how long a model's declared input modalities are
// served without reading them again, should a write ever change them
// without announcing a model change.
const modalityMaxAge = 30 * time.Second

// modelModalities caches each model's declared input modalities for the
// chat input check, so a chat request reads neither its deployment nor its
// model. The cache holds while the model topic's bus generation stands (an
// edit of a model's declarations announces one); any model change empties it.
type modelModalities struct {
	store *store.Store
	now   func() time.Time
	mu    sync.Mutex
	gen   uint64
	byID  map[uint]modalityEntry
}

type modalityEntry struct {
	supported []string
	at        time.Time
}

func (m *modelModalities) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// forModel is the input modalities model declares; none (nil) when it
// declares none or they cannot be read as a list.
func (m *modelModalities) forModel(modelID uint) ([]string, error) {
	gen := uint64(0)
	cacheable := m.store != nil && m.store.Bus != nil
	if cacheable {
		gen = m.store.Bus.Generation("model")
	}
	now := m.clock()
	if cacheable {
		m.mu.Lock()
		if m.gen != gen {
			m.gen, m.byID = gen, nil
		}
		if e, ok := m.byID[modelID]; ok && now.Sub(e.at) < modalityMaxAge {
			m.mu.Unlock()
			return e.supported, nil
		}
		m.mu.Unlock()
	}
	model, err := m.store.GetModel(modelID)
	if err != nil {
		return nil, err
	}
	var supported []string
	if json.Unmarshal([]byte(model.InputModalities), &supported) != nil || len(supported) == 0 {
		supported = nil
	}
	if cacheable {
		m.mu.Lock()
		if m.gen == gen {
			if m.byID == nil {
				m.byID = map[uint]modalityEntry{}
			}
			m.byID[modelID] = modalityEntry{supported: supported, at: now}
		}
		m.mu.Unlock()
	}
	return supported, nil
}
