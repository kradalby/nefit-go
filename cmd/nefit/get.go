package main

import (
	"context"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"

	"github.com/kradalby/nefit-go/protocol"
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
  /dhwCircuits/dhwA/dhwOperationManualMode    - Hot water in manual mode
  /dhwCircuits/dhwA/dhwOperationClockMode     - Hot water in clock mode
  /heatingCircuits/hc1/usermode               - User mode

Examples:
  nefit get /ecus/rrc/uiStatus
  nefit --pretty get /system/sensors/temperatures/outdoor_t1`,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("uri required: nefit get <uri>")
		}

		uri := args[0]
		// Checked before logging in, so a typo costs no Bosch session.
		if err := protocol.ValidateURI(uri); err != nil {
			return err
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

		data, err := c.Get(reqCtx, uri)
		if err != nil {
			return fmt.Errorf("GET request failed: %w", err)
		}

		return printJSON(data)
	},
}
