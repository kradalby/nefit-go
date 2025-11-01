package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/peterbourgon/ff/v3/ffcli"
)

var (
	setFlagSet = flag.NewFlagSet("set", flag.ExitOnError)
)

var setCmd = &ffcli.Command{
	Name:       "set",
	ShortUsage: "nefit set <subcommand> <value>",
	ShortHelp:  "Set values on the thermostat (WRITE operations)",
	LongHelp: `Set values on the thermostat.

⚠️  WARNING: These perform WRITE operations on your thermostat!
    Test carefully with small changes first.

Available subcommands:
  temperature <value>  - Set the manual temperature setpoint
  user-mode <mode>     - Set user mode (manual or clock)

Examples:
  nefit set temperature 21.5
  nefit set temperature 22
  nefit set user-mode manual
  nefit set user-mode clock`,
	FlagSet: setFlagSet,
	Subcommands: []*ffcli.Command{
		setTemperatureCmd,
		setUserModeCmd,
	},
	Exec: func(ctx context.Context, args []string) error {
		return flag.ErrHelp
	},
}

var setTemperatureCmd = &ffcli.Command{
	Name:       "temperature",
	ShortUsage: "nefit set temperature <value>",
	ShortHelp:  "Set the manual temperature setpoint",
	LongHelp: `Set the manual temperature setpoint in degrees Celsius.

⚠️  This will:
  1. Set the manual temperature
  2. Enable manual override
  3. Switch to manual mode

Start with small changes (±0.5°C) to verify it works on your system.

Examples:
  nefit set temperature 21.5
  nefit set temperature 22`,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("temperature value required: nefit set temperature <value>")
		}

		temp, err := strconv.ParseFloat(args[0], 64)
		if err != nil {
			return fmt.Errorf("invalid temperature value: %w", err)
		}

		if temp < 5 || temp > 30 {
			return fmt.Errorf("temperature %v is outside reasonable range (5-30°C)", temp)
		}

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

		if *verbose {
			fmt.Fprintf(os.Stderr, "Setting temperature to %.1f°C...\n", temp)
		}

		if err := c.SetTemperature(reqCtx, temp); err != nil {
			return fmt.Errorf("failed to set temperature: %w", err)
		}

		fmt.Printf("OK - Temperature set to %.1f°C\n", temp)
		return nil
	},
}

var setUserModeCmd = &ffcli.Command{
	Name:       "user-mode",
	ShortUsage: "nefit set user-mode <manual|clock>",
	ShortHelp:  "Set the user mode",
	LongHelp: `Set the user mode to either 'manual' or 'clock'.

  manual - Manual mode (you set temperature directly)
  clock  - Clock/program mode (follows the programmed schedule)

Examples:
  nefit set user-mode manual
  nefit set user-mode clock`,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("mode required: nefit set user-mode <manual|clock>")
		}

		mode := args[0]
		if mode != "manual" && mode != "clock" {
			return fmt.Errorf("invalid mode %q (must be 'manual' or 'clock')", mode)
		}

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

		if *verbose {
			fmt.Fprintf(os.Stderr, "Setting user mode to %s...\n", mode)
		}

		if err := c.SetUserMode(reqCtx, mode); err != nil {
			return fmt.Errorf("failed to set user mode: %w", err)
		}

		fmt.Printf("OK - User mode set to %s\n", mode)
		return nil
	},
}
