package api

import (
	"encoding/json"
	"os"
	"testing"

	"gent/internal/db"
)

func newTestHandlers(t *testing.T) (*Handlers, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "gent-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	database, err := db.OpenSQLite(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return NewHandlers(database, nil), func() {
		database.Close()
		os.Remove(f.Name())
	}
}

func TestHandle_UnknownAction(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()

	reply := h.Handle(Envelope{Action: "does_not_exist"})
	if reply.OK {
		t.Error("expected ok=false for unknown action")
	}
}

func TestHandle_PutAndListDefinitions(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()

	payload, _ := json.Marshal(map[string]interface{}{
		"name": "pipeline",
		"steps": []map[string]interface{}{
			{"id": "s1", "call": map[string]interface{}{"type": "rest", "endpoint": "http://localhost/x"}, "switch": []map[string]interface{}{{"goto": "end"}}},
		},
	})

	put := h.Handle(Envelope{Action: "put_definition", Payload: payload})
	if !put.OK {
		t.Fatalf("put_definition failed: %s", put.Error)
	}

	list := h.Handle(Envelope{Action: "list_definitions"})
	if !list.OK {
		t.Fatalf("list_definitions failed: %s", list.Error)
	}

	var defs []DefinitionSummary
	if err := json.Unmarshal(list.Data, &defs); err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "pipeline" || defs[0].Version != 1 {
		t.Errorf("unexpected definitions: %+v", defs)
	}
}

func TestHandle_StartAndGetInstance(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()

	// Register definition first.
	defPayload, _ := json.Marshal(map[string]interface{}{
		"name": "p",
		"steps": []map[string]interface{}{
			{"id": "s1", "call": map[string]interface{}{"type": "rest", "endpoint": "http://localhost/x"}, "switch": []map[string]interface{}{{"goto": "end"}}},
		},
	})
	h.Handle(Envelope{Action: "put_definition", Payload: defPayload})

	// Start instance.
	startPayload, _ := json.Marshal(StartInstanceReq{Process: "p"})
	start := h.Handle(Envelope{Action: "start_instance", Payload: startPayload})
	if !start.OK {
		t.Fatalf("start_instance failed: %s", start.Error)
	}

	var resp StartInstanceResp
	json.Unmarshal(start.Data, &resp)
	if resp.ID == "" {
		t.Fatal("expected instance ID")
	}

	// Get instance.
	get := h.Handle(Envelope{Action: "get_instance", ID: resp.ID})
	if !get.OK {
		t.Fatalf("get_instance failed: %s", get.Error)
	}

	var inst InstanceStatusResp
	json.Unmarshal(get.Data, &inst)
	if inst.ID != resp.ID {
		t.Errorf("expected id %q, got %q", resp.ID, inst.ID)
	}
	if inst.Status != "running" {
		t.Errorf("expected status running, got %q", inst.Status)
	}
}

func TestHandle_ValidationErrors(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()

	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{
			name:    "rest call without endpoint",
			payload: `{"name":"p","steps":[{"id":"s1","call":{"type":"rest"}}]}`,
			wantErr: "call.endpoint is required",
		},
		{
			name:    "unknown call type",
			payload: `{"name":"p","steps":[{"id":"s1","call":{"type":"ftp","endpoint":"x"}}]}`,
			wantErr: "call.type must be one of",
		},
		{
			name:    "missing process name",
			payload: `{}`,
			wantErr: "name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reply := h.Handle(Envelope{Action: "put_definition", Payload: json.RawMessage(tt.payload)})
			if reply.OK {
				t.Error("expected ok=false")
			}
			if reply.Error == "" || !containsString(reply.Error, tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, reply.Error)
			}
		})
	}
}

func containsString(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := range s {
			if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
