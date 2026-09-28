package jmapapi

import (
	"encoding/json"
	"testing"
)

// TestSetResponseShapes pins FR-J.4: absent collections are null (never
// [] or {}), Updated is an array of ids, and SetErrors carry per-id
// objects with type/properties/description.
func TestSetResponseShapes(t *testing.T) {
	resp := NewSetResponse("personal", "2")
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"created", "updated", "destroyed", "notCreated", "notUpdated", "notDestroyed"} {
		if v, present := decoded[key]; !present || v != nil {
			t.Errorf("%s = %v (present=%v), want explicit null", key, v, present)
		}
	}
	if decoded["oldState"] != nil {
		t.Errorf("oldState = %v, want null when unknown", decoded["oldState"])
	}
	if decoded["newState"] != "2" || decoded["accountId"] != "personal" {
		t.Errorf("identity fields = %v / %v", decoded["accountId"], decoded["newState"])
	}
}

func TestSetResponsePopulated(t *testing.T) {
	resp := NewSetResponse("personal", "3")
	desc := "IMAP said no"
	resp.Updated = []string{"em-1", "em-2"}
	resp.NotUpdated = map[string]SetError{
		"em-3": NewSetError("serverFail", nil, desc),
		"em-4": NewSetError("invalidProperties", []string{"subject"}, ""),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	updated, ok := decoded["updated"].([]any)
	if !ok || len(updated) != 2 || updated[0] != "em-1" {
		t.Fatalf("updated = %v, want an array of ids", decoded["updated"])
	}
	notUpdated := decoded["notUpdated"].(map[string]any)
	e3 := notUpdated["em-3"].(map[string]any)
	if e3["type"] != "serverFail" || e3["description"] != desc {
		t.Errorf("em-3 error = %v", e3)
	}
	if _, present := e3["properties"]; present {
		t.Errorf("properties must be omitted when unset: %v", e3)
	}
	e4 := notUpdated["em-4"].(map[string]any)
	props := e4["properties"].([]any)
	if len(props) != 1 || props[0] != "subject" {
		t.Errorf("em-4 properties = %v", props)
	}
	if _, present := e4["description"]; present {
		t.Errorf("description must be omitted when empty: %v", e4)
	}
}
