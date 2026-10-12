// SPDX-License-Identifier: LGPL-3.0-only
package cprnative

import (
	"bytes"
	"encoding/json"
	"errors"
)

// FilterCatalog applies only the host's model access policy, never model
// capability defaults or compatibility mappings. If no entry is removed the
// original bytes (and therefore the upstream ETag) remain usable.
// When filtering is necessary each retained entry is kept as RawMessage.
func FilterCatalog(body []byte, allowed func(string) bool) ([]byte, bool, error) {
	if err := ValidateJSON(body); err != nil {
		return nil, false, err
	}
	root := rawObject(body)
	if root == nil {
		return nil, false, errors.New("CPR model catalog must be an object")
	}
	key := "models"
	if _, exists := root[key]; !exists {
		key = "data"
	}
	raw, exists := root[key]
	if !exists {
		return nil, false, errors.New("CPR model catalog lacks models/data array")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, errors.New("CPR model catalog array is null")
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil, false, errors.New("CPR model catalog array is invalid")
	}
	if allowed == nil {
		return body, false, nil
	}
	kept := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		model := rawObject(entry)
		id := rawString(model, "slug")
		if id == "" {
			id = rawString(model, "id")
		}
		if id != "" && allowed(id) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(entries) {
		return body, false, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, false, err
	}
	root[key] = encoded
	out, err := json.Marshal(root)
	return out, true, err
}
