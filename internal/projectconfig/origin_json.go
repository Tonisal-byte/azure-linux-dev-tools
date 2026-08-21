// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package projectconfig

import (
	"encoding/json"
	"fmt"
)

type originJSON Origin

var _ json.Marshaler = Origin{}

// MarshalJSON emits the configured script filename instead of its absolute internal path.
func (o Origin) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(struct {
		originJSON
		Script string `json:"script,omitempty"`
	}{
		originJSON: originJSON(o),
		Script:     o.EffectiveScriptName(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling source origin:\n%w", err)
	}

	return data, nil
}
