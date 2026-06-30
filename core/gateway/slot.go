package gateway

import (
	"encoding/json"
	"net/http"
	"sync"
)

// UISlot maps a host-page slot to an extension micro-frontend component.
type UISlot struct {
	SlotName       string `json:"slot_name"`
	ExtensionKey   string `json:"extension_key"`
	ComponentEntry string `json:"component_entry"`
}

// SlotRegistry is the concurrency-safe store of declared UI slots. Extensions
// register slots from their manifest at registration time; the Qiankun host
// queries them to inline component-level micro-frontends.
type SlotRegistry struct {
	mu    sync.RWMutex
	slots map[string][]UISlot // keyed by extension key for easy de-registration
}

func NewSlotRegistry() *SlotRegistry {
	return &SlotRegistry{slots: make(map[string][]UISlot)}
}

// Register replaces the slot set for an extension.
func (s *SlotRegistry) Register(extKey string, slots []UISlot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slots[extKey] = slots
}

// Unregister drops all slots owned by an extension.
func (s *SlotRegistry) Unregister(extKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slots, extKey)
}

// List returns a flattened snapshot of all registered slots.
func (s *SlotRegistry) List() []UISlot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UISlot, 0)
	for _, slots := range s.slots {
		out = append(out, slots...)
	}
	return out
}

// Handler serves GET /api/system/ui/slots.
func (s *SlotRegistry) Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.List())
}
