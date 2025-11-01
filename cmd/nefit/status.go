package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var (
	statusFlagSet       = flag.NewFlagSet("status", flag.ExitOnError)
	statusSkipOutdoor   = statusFlagSet.Bool("skip-outdoor", false, "Skip fetching outdoor temperature")
)

var statusCmd = &ffcli.Command{
	Name:       "status",
	ShortUsage: "nefit status [flags]",
	ShortHelp:  "Get complete system status",
	LongHelp: `Get the complete system status including:
  - Indoor temperature
  - Setpoint temperature
  - Outdoor temperature (unless --skip-outdoor)
  - User mode (manual/clock)
  - Boiler status
  - Hot water status
  - And more...

Example:
  nefit status
  nefit status --pretty
  nefit status --skip-outdoor`,
	FlagSet: statusFlagSet,
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

		status, err := c.Status(reqCtx, !*statusSkipOutdoor)
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}

		return printJSON(status)
	},
}
