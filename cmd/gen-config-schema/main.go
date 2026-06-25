package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"

	"github.com/solidDoWant/tape-archiver/internal/config"
)

func main() {
	r := &jsonschema.Reflector{
		AllowAdditionalProperties: false,
	}
	schema := r.Reflect(&config.Config{})

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-config-schema: marshal: %v\n", err)
		os.Exit(1)
	}

	data = append(data, '\n')

	if len(os.Args) > 1 {
		if err := os.WriteFile(os.Args[1], data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gen-config-schema: write %s: %v\n", os.Args[1], err)
			os.Exit(1)
		}

		return
	}

	if _, err := os.Stdout.Write(data); err != nil {
		fmt.Fprintf(os.Stderr, "gen-config-schema: stdout: %v\n", err)
		os.Exit(1)
	}
}
