package main

import (
	"context"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var getCmd = &ffcli.Command{
	Name:       "get",
	ShortUsage: "nefit get <uri>",
	ShortHelp:  "Perform a raw GET request",
	LongHelp: `Perform a raw GET request to any endpoint.

This is useful for accessing endpoints that don't have dedicated commands yet.

Common URIs:
  /ecus/rrc/uiStatus                          - System status
  /system/appliance/systemPressure            - System pressure
  /system/sensors/temperatures/outdoor_t1     - Outdoor temperature
  /dhwCircuits/dhwA/dhwOperationMode          - Hot water mode
  /ecus/rrc/usermode                          - User mode

Examples:
  nefit get /ecus/rrc/uiStatus
  nefit get /system/sensors/temperatures/outdoor_t1 --pretty`,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("uri required: nefit get <uri>")
		}

		uri := args[0]

		c, err := createClient()
		if err != nil {
			return err
		}
		defer c.Close()

		if err := connectClient(c); err != nil {
			return err
		}

		reqCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()

		data, err := c.Get(reqCtx, uri)
		if err != nil {
			return fmt.Errorf("GET request failed: %w", err)
		}

		return printJSON(data)
	},
}
