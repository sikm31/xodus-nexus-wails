package shared

import "encoding/json"

// Clone deep-copies a JSON-shaped value, like structuredClone for IPC data.
func Clone[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(encoded, &out); err != nil {
		panic(err)
	}
	return out
}
