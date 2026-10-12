// SPDX-License-Identifier: LGPL-3.0-only
package cprnative

import (
	"encoding/json"
	"errors"
)

// FilterCatalog preserves the CPR representation byte-for-byte unless model
// authorization actually removes an entry. Never add model capabilities or
// rewrite use_responses_lite, tool metadata or model names.
func FilterCatalog(body []byte, allow func(string) bool) ([]byte, bool, error) {
	if err := ValidateJSON(body); err != nil {
		return nil, false, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, false, errors.New("CPR model catalog must be a JSON object")
	}
	field, idKey := "models", "slug"
	if _, ok := envelope[field]; !ok {
		field, idKey = "data", "id"
	}
	var entries []json.RawMessage
	if raw, ok := envelope[field]; !ok || string(raw) == "null" {
		return nil, false, errors.New("CPR model catalog has no model array")
	} else if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false, err
	}
	if allow == nil {
		return body, false, nil
	}
	kept := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		var model map[string]json.RawMessage
		var id string
		if json.Unmarshal(entry, &model) == nil {
			_ = json.Unmarshal(model[idKey], &id)
		}
		if id != "" && allow(id) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == len(entries) {
		return body, false, nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, false, err
	}
	envelope[field] = raw
	out, err := json.Marshal(envelope)
	return out, true, err
}
