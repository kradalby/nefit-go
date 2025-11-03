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
	serialNumber := os.Getenv("NEFIT_SERIAL_NUMBER")
	accessKey := os.Getenv("NEFIT_ACCESS_KEY")
	password := os.Getenv("NEFIT_PASSWORD")

	if serialNumber == "" || accessKey == "" || password == "" {
		log.Fatal("Please set NEFIT_SERIAL_NUMBER, NEFIT_ACCESS_KEY, and NEFIT_PASSWORD environment variables")
	}

	if len(os.Args) < 2 {
		log.Fatal("Usage: set-temperature <temperature>\nExample: set-temperature 21.5")
	}

	temp, err := strconv.ParseFloat(os.Args[1], 64)
	if err != nil {
		log.Fatalf("Invalid temperature: %v", err)
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
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("Connecting to Nefit Easy...")
	if err := c.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	fmt.Println("Connected successfully!")

	fmt.Println("\nFetching current status...")
	status, err := c.Status(ctx, false)
	if err != nil {
		log.Fatalf("Failed to get status: %v", err)
	}

	fmt.Printf("Current temperature: %.1f°C\n", status.InHouseTemp)
	fmt.Printf("Current setpoint: %.1f°C\n", status.TempSetpoint)

	fmt.Printf("\nSetting temperature to %.1f°C...\n", temp)
	if err := c.SetTemperature(ctx, temp); err != nil {
		log.Fatalf("Failed to set temperature: %v", err)
	}

	fmt.Println("Temperature set successfully!")

	time.Sleep(2 * time.Second)
	status, err = c.Status(ctx, false)
	if err == nil {
		fmt.Printf("New setpoint: %.1f°C\n", status.TempManualSetpoint)
	}
}
