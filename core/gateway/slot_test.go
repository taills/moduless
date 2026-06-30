package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSlotRegistry(t *testing.T) {
	reg := NewSlotRegistry()
	reg.Register("ext1", []UISlot{{SlotName: "header", ExtensionKey: "ext1", ComponentEntry: "/extensions/ext1/header.js"}})
	reg.Register("ext2", []UISlot{{SlotName: "footer", ExtensionKey: "ext2", ComponentEntry: "/extensions/ext2/footer.js"}})

	if got := len(reg.List()); got != 2 {
		t.Fatalf("expected 2 slots, got %d", got)
	}

	reg.Unregister("ext1")
	if got := len(reg.List()); got != 1 {
		t.Fatalf("expected 1 slot after unregister, got %d", got)
	}

	req := httptest.NewRequest("GET", "/api/system/ui/slots", nil)
	rr := httptest.NewRecorder()
	reg.Handler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var slots []UISlot
	if err := json.Unmarshal(rr.Body.Bytes(), &slots); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(slots) != 1 || slots[0].SlotName != "footer" {
		t.Fatalf("unexpected slots payload: %+v", slots)
	}
}
