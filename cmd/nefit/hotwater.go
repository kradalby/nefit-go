package main

import (
	"context"
	"fmt"
	"os"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var hotWaterCmd = &ffcli.Command{
	Name:       "hot-water",
	ShortUsage: "nefit hot-water [on|off]",
	ShortHelp:  "Get or set hot water supply",
	LongHelp: `Get or set the hot water supply status.

Without arguments, shows the current status.
With 'on' or 'off', sets the status (WRITE operation).

Examples:
  nefit hot-water           # Get current status
  nefit hot-water on        # Turn on hot water
  nefit hot-water off       # Turn off hot water`,
	Exec: func(ctx context.Context, args []string) error {
		// Checked before logging in, so a typo costs no Bosch session.
		var enabled bool
		if len(args) > 0 {
			switch args[0] {
			case "on":
				enabled = true
			case "off":
			default:
				return fmt.Errorf("invalid argument %q (must be 'on' or 'off')", args[0])
			}
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

		// No arguments - get status
		if len(args) == 0 {
			active, err := c.HotWaterSupply(reqCtx)
			if err != nil {
				return fmt.Errorf("failed to get hot water status: %w", err)
			}

			status := "off"
			if active {
				status = "on"
			}
			fmt.Printf("Hot water: %s\n", status)
			return nil
		}

		arg := args[0]
		if *verbose {
			fmt.Fprintf(os.Stderr, "Setting hot water to %s...\n", arg)
		}

		if err := c.SetHotWaterSupply(reqCtx, enabled); err != nil {
			return fmt.Errorf("failed to set hot water: %w", err)
		}

		fmt.Printf("OK - Hot water set to %s\n", arg)
		return nil
	},
}
