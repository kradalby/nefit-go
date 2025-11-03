package client

import (
	"context"
	"fmt"

	"github.com/kradalby/nefit-go/types"
)

// Status retrieves the complete system status including temperatures, modes, and boiler state.
// If includeOutdoorTemp is true, an additional request is made to fetch outdoor temperature data.
func (c *Client) Status(ctx context.Context, includeOutdoorTemp bool) (*types.Status, error) {
	statusData, err := c.Get(ctx, types.URIStatus)
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	statusMap, ok := statusData.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected status response type: %T", statusData)
	}

	valueMap, ok := statusMap["value"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("status response missing 'value' field")
	}

	status := &types.Status{
		UserMode:                 getString(valueMap, "UMD"),
		ClockProgram:             getString(valueMap, "CPM"),
		InHouseStatus:            getString(valueMap, "IHS"),
		InHouseTemp:              getFloat(valueMap, "IHT"),
		HotWaterActive:           parseBoolean(getString(valueMap, "DHW")),
		BoilerIndicator:          parseBoilerIndicator(getString(valueMap, "BAI")),
		Control:                  getString(valueMap, "CTR"),
		TempOverrideDuration:     getInt(valueMap, "TOD"),
		CurrentSwitchpoint:       getInt(valueMap, "CSP"),
		PSActive:                 parseBoolean(getString(valueMap, "ESI")),
		PowersaveMode:            parseBoolean(getString(valueMap, "ESI")),
		FPActive:                 parseBoolean(getString(valueMap, "FPA")),
		FireplaceMode:            parseBoolean(getString(valueMap, "FPA")),
		TempOverride:             parseBoolean(getString(valueMap, "TOR")),
		HolidayMode:              parseBoolean(getString(valueMap, "HMD")),
		BoilerBlock:              parseBoolean(getString(valueMap, "BBE")),
		BoilerLock:               parseBoolean(getString(valueMap, "BLE")),
		BoilerMaintenance:        parseBoolean(getString(valueMap, "BMR")),
		TempSetpoint:             getFloat(valueMap, "TSP"),
		TempOverrideTempSetpoint: getFloat(valueMap, "TOT"),
		TempManualSetpoint:       getFloat(valueMap, "MMT"),
		HEDEnabled:               parseBoolean(getString(valueMap, "HED_EN")),
		HEDDeviceAtHome:          parseBoolean(getString(valueMap, "HED_DEV")),
	}

	if includeOutdoorTemp {
		outdoorData, err := c.Get(ctx, types.URIOutdoorTemp)
		if err == nil {
			if outdoorMap, ok := outdoorData.(map[string]interface{}); ok {
				status.OutdoorTemp = getFloat(outdoorMap, "value")
				status.OutdoorSourceType = getString(outdoorMap, "srcType")
			}
		}
	}

	return status, nil
}

// Pressure retrieves the system pressure reading in bar.
// Low pressure may indicate a leak or the need to refill the system.
func (c *Client) Pressure(ctx context.Context) (*types.Pressure, error) {
	data, err := c.Get(ctx, types.URIPressure)
	if err != nil {
		return nil, fmt.Errorf("failed to get pressure: %w", err)
	}

	dataMap, ok := data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected pressure response type: %T", data)
	}

	pressure := &types.Pressure{
		Pressure: getFloat(dataMap, "value"),
		Unit:     getString(dataMap, "unitOfMeasure"),
		MinValue: getFloat(dataMap, "minValue"),
		MaxValue: getFloat(dataMap, "maxValue"),
	}

	return pressure, nil
}

// SetTemperature sets the manual temperature setpoint and enables manual override mode.
// This requires three separate API calls to fully configure the temperature override.
func (c *Client) SetTemperature(ctx context.Context, temperature float64) error {
	data := map[string]interface{}{
		"value": temperature,
	}

	if err := c.Put(ctx, types.URIManualSetpoint, data); err != nil {
		return fmt.Errorf("failed to set manual temperature: %w", err)
	}

	overrideData := map[string]string{
		"value": "on",
	}
	if err := c.Put(ctx, types.URIManualTempOverrideStatus, overrideData); err != nil {
		return fmt.Errorf("failed to enable manual override: %w", err)
	}

	if err := c.Put(ctx, types.URIManualTempOverrideTemp, data); err != nil {
		return fmt.Errorf("failed to set override temperature: %w", err)
	}

	return nil
}

// SetUserMode switches between "manual" and "clock" (scheduled) heating modes.
func (c *Client) SetUserMode(ctx context.Context, mode string) error {
	if mode != "manual" && mode != "clock" {
		return fmt.Errorf("invalid mode: %s (must be 'manual' or 'clock')", mode)
	}

	data := map[string]string{
		"value": mode,
	}

	return c.Put(ctx, types.URIUserMode, data)
}

// SetHotWaterSupply enables or disables hot water supply.
// The API endpoint used depends on the current user mode (manual vs clock).
func (c *Client) SetHotWaterSupply(ctx context.Context, enabled bool) error {
	status, err := c.Status(ctx, false)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	endpoint := types.URIHotWaterManualMode
	if status.UserMode == "clock" {
		endpoint = types.URIHotWaterClockMode
	}

	value := "off"
	if enabled {
		value = "on"
	}

	data := map[string]string{
		"value": value,
	}

	return c.Put(ctx, endpoint, data)
}

// HotWaterSupply retrieves the current hot water supply status (on/off).
// The API endpoint used depends on the current user mode (manual vs clock).
func (c *Client) HotWaterSupply(ctx context.Context) (bool, error) {
	status, err := c.Status(ctx, false)
	if err != nil {
		return false, fmt.Errorf("failed to get status: %w", err)
	}

	endpoint := types.URIHotWaterManualMode
	if status.UserMode == "clock" {
		endpoint = types.URIHotWaterClockMode
	}

	data, err := c.Get(ctx, endpoint)
	if err != nil {
		return false, fmt.Errorf("failed to get hot water supply: %w", err)
	}

	dataMap, ok := data.(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("unexpected response type: %T", data)
	}

	value := getString(dataMap, "value")
	return value == "on", nil
}

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return ""
}

func getFloat(m map[string]interface{}, key string) float64 {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return v
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case string:
			var f float64
			_, _ = fmt.Sscanf(v, "%f", &f)
			return f
		}
	}
	return 0
}

func getInt(m map[string]interface{}, key string) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		case float32:
			return int(v)
		case string:
			var i int
			_, _ = fmt.Sscanf(v, "%d", &i)
			return i
		}
	}
	return 0
}

func parseBoolean(val string) bool {
	return val == "on"
}

func parseBoilerIndicator(val string) string {
	switch val {
	case "CH":
		return "central heating"
	case "HW":
		return "hot water"
	case "No":
		return "off"
	default:
		return val
	}
}
