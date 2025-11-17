package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/kradalby/nefit-go/client"
)

func main() {
	serialNumber := os.Getenv("NEFIT_SERIAL_NUMBER")
	accessKey := os.Getenv("NEFIT_ACCESS_KEY")
	password := os.Getenv("NEFIT_PASSWORD")

	if serialNumber == "" || accessKey == "" || password == "" {
		log.Fatal("Please set NEFIT_SERIAL_NUMBER, NEFIT_ACCESS_KEY, and NEFIT_PASSWORD environment variables")
	}

	config := client.Config{
		SerialNumber: serialNumber,
		AccessKey:    accessKey,
		Password:     password,
	}

	c, err := client.NewClient(config)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer c.Close() //nolint:errcheck

	// Enable debug logging to see detailed request/response information
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	c.SetLogger(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("Connecting to Nefit Easy with debug logging enabled...")
	fmt.Println("Watch the logs below to see detailed request/response information")
	fmt.Println("========================================")

	if err := c.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}

	// Example 1: GET request (fetch status)
	fmt.Println("\n=== Example 1: GET Request (Status) ===")
	status, err := c.Status(ctx, true)
	if err != nil {
		log.Fatalf("Failed to get status: %v", err)
	}
	fmt.Printf("\nResult: User mode = %s, In-house temp = %.1f°C\n", status.UserMode, status.InHouseTemp)

	// Example 2: PUT request with valid data (set user mode to manual)
	fmt.Println("\n=== Example 2: PUT Request (Valid) ===")
	fmt.Println("Setting user mode to 'manual'...")
	if err := c.SetUserMode(ctx, "manual"); err != nil {
		log.Fatalf("Failed to set user mode: %v", err)
	}
	fmt.Println("\nResult: User mode set successfully")

	// Example 3: Demonstrate what happens with invalid data
	fmt.Println("\n=== Example 3: PUT Request (Invalid - for demonstration) ===")
	fmt.Println("Attempting to set user mode to 'off' (which is invalid)...")
	if err := c.SetUserMode(ctx, "off"); err != nil {
		fmt.Printf("\nExpected error occurred: %v\n", err)
		fmt.Println("Notice: The library caught this BEFORE sending to the API")
	}

	// Example 4: Raw PUT to demonstrate what debug logging shows
	fmt.Println("\n=== Example 4: Raw PUT Request ===")
	fmt.Println("Setting temperature to 20.5°C using raw Put()...")
	data := map[string]interface{}{
		"value": 20.5,
	}
	if err := c.Put(ctx, "/heatingCircuits/hc1/temperatureRoomManual", data); err != nil {
		log.Fatalf("Failed to set temperature: %v", err)
	}
	fmt.Println("\nResult: Temperature set successfully")

	fmt.Println("\n========================================")
	fmt.Println("\nDebug logging demonstration complete!")
	fmt.Println("\nKey debug log fields to notice:")
	fmt.Println("  - 'json_data': The actual JSON payload (decrypted)")
	fmt.Println("  - 'encrypted_payload_length': Size of encrypted data")
	fmt.Println("  - 'status_code': HTTP response code")
	fmt.Println("  - 'attempt': Retry attempt number (if any)")
	fmt.Println("  - 'backoff': Wait time before retry")
	fmt.Println("\nSee API_NOTES.md for more information on debugging and troubleshooting.")
}
