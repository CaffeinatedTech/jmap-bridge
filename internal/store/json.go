package store

import "encoding/json"

// jsonMarshal/jsonUnmarshal keep encoding/json usage in one place and
// errors wrapped at the call sites that care.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

// jsonString marshals v as a JSON string column value.
func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
