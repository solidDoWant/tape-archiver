package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// LoadFile reads a run-config JSON file from path and returns a validated
// Config. It is the single client-side gate a config passes through before it
// is submitted as a Temporal workflow payload.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	return Parse(data)
}

// Parse decodes raw run-config JSON into a validated Config, rejecting unknown
// fields. It returns a human-readable error on malformed JSON, an unrecognized
// field, or any validation failure.
//
// Validation is performed against the Go types — the source of truth the
// committed JSON schema (schemas/run-config.schema.json) is generated from.
// DisallowUnknownFields reproduces the schema's additionalProperties:false so a
// misspelled key is rejected rather than silently ignored, and Validate adds
// the cross-field semantic rules the structural schema cannot express.
func Parse(data []byte) (*Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var config Config
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}
