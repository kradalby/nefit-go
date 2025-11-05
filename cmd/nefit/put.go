package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var putCmd = &ffcli.Command{
	Name:       "put",
	ShortUsage: "nefit put <uri> <json-data>",
	ShortHelp:  "Perform a raw PUT request (WRITE operation - use carefully!)",
	LongHelp: `Perform a raw PUT request to any endpoint.

⚠️  WARNING: This performs WRITE operations on your thermostat!
    Test carefully with small changes first.

The data should be valid JSON, typically in the format:
  {"value": <your-value>}

Examples:
  nefit put /heatingCircuits/hc1/temperatureRoomManual '{"value":21.5}'
  nefit put /ecus/rrc/usermode '{"value":"manual"}'

For simple values, you can also use:
  nefit set temperature 21.5    # Easier than using PUT directly`,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("uri and data required: nefit put <uri> <json-data>")
		}

		uri := args[0]
		jsonData := args[1]

		// Parse JSON to validate it
		var data interface{}
		if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
			return fmt.Errorf("invalid JSON data: %w", err)
		}

		c, err := createClient()
		if err != nil {
			return err
		}
		defer c.Close() //nolint:errcheck

		if err := connectClient(c); err != nil {
			return err
		}

		reqCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()

		if err := c.Put(reqCtx, uri, data); err != nil {
			return fmt.Errorf("PUT request failed: %w", err)
		}

		fmt.Println("OK")
		return nil
	},
}
