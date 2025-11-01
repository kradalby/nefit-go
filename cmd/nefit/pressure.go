package main

import (
	"context"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var pressureCmd = &ffcli.Command{
	Name:       "pressure",
	ShortUsage: "nefit pressure",
	ShortHelp:  "Get system pressure",
	LongHelp: `Get the current system pressure.

Example:
  nefit pressure
  nefit pressure --pretty`,
	Exec: func(ctx context.Context, args []string) error {
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

		pressure, err := c.Pressure(reqCtx)
		if err != nil {
			return fmt.Errorf("failed to get pressure: %w", err)
		}

		return printJSON(pressure)
	},
}
