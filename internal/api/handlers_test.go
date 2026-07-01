package api

import (
	"os"
	"testing"

	"genroc/internal/db"
)

func newTestHandlers(t *testing.T) (*Handlers, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "genroc-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	database, err := db.OpenSQLite(f.Name(), "")
	if err != nil {
		t.Fatal(err)
	}
	return NewHandlers(database, nil), func() {
		database.Close()
		os.Remove(f.Name())
	}
}

// TestHandle_UnknownAction covers the envelope-dispatch default — there is no HTTP
// route equivalent, so this stays a Go test. CRUD and validation behavior is
// covered end-to-end by the TypeScript suite in tests/integration.
func TestHandle_UnknownAction(t *testing.T) {
	h, cleanup := newTestHandlers(t)
	defer cleanup()

	reply := h.Handle(Envelope{Action: "does_not_exist"})
	if reply.OK {
		t.Error("expected ok=false for unknown action")
	}
}
