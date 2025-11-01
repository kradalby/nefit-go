package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/kradalby/nefit-go/client"
)

func main() {
	// Read credentials from environment variables
	serialNumber := os.Getenv("NEFIT_SERIAL_NUMBER")
	accessKey := os.Getenv("NEFIT_ACCESS_KEY")
	password := os.Getenv("NEFIT_PASSWORD")

	if serialNumber == "" || accessKey == "" || password == "" {
		log.Fatal("Please set NEFIT_SERIAL_NUMBER, NEFIT_ACCESS_KEY, and NEFIT_PASSWORD environment variables")
	}

	// Get desired temperature from command line argument
	if len(os.Args) < 2 {
		log.Fatal("Usage: set-temperature <temperature>\nExample: set-temperature 21.5")
	}

	temp, err := strconv.ParseFloat(os.Args[1], 64)
	if err != nil {
		log.Fatalf("Invalid temperature: %v", err)
	}

	// Create client configuration
	config := client.Config{
		SerialNumber: serialNumber,
		AccessKey:    accessKey,
		Password:     password,
	}

	// Create client
	c, err := client.NewSimpleClient(config)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer c.Close()

	// Connect to the Nefit Easy backend
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("Connecting to Nefit Easy...")
	if err := c.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	fmt.Println("Connected successfully!")

	// Get current status
	fmt.Println("\nFetching current status...")
	status, err := c.Status(ctx, false)
	if err != nil {
		log.Fatalf("Failed to get status: %v", err)
	}

	fmt.Printf("Current temperature: %.1f°C\n", status.InHouseTemp)
	fmt.Printf("Current setpoint: %.1f°C\n", status.TempSetpoint)

	// Set new temperature
	fmt.Printf("\nSetting temperature to %.1f°C...\n", temp)
	if err := c.SetTemperature(ctx, temp); err != nil {
		log.Fatalf("Failed to set temperature: %v", err)
	}

	fmt.Println("Temperature set successfully!")

	// Verify the change
	time.Sleep(2 * time.Second) // Give the system time to update
	status, err = c.Status(ctx, false)
	if err == nil {
		fmt.Printf("New setpoint: %.1f°C\n", status.TempManualSetpoint)
	}
}
