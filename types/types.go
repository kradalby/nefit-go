package types

// Status contains comprehensive heating system state including temperatures, modes, and diagnostics.
type Status struct {
	UserMode                 string  `json:"user_mode"`                    // "manual" or "clock"
	ClockProgram             string  `json:"clock_program"`                // Current program mode
	InHouseStatus            string  `json:"in_house_status"`              // Status of in-house sensor
	InHouseTemp              float64 `json:"in_house_temp"`                // Current indoor temperature
	HotWaterActive           bool    `json:"hot_water_active"`             // Hot water system status
	BoilerIndicator          string  `json:"boiler_indicator"`             // "CH" (central heating), "HW" (hot water), "No" (off)
	Control                  string  `json:"control"`                      // Control mode
	TempOverrideDuration     int     `json:"temp_override_duration"`       // Minutes
	CurrentSwitchpoint       int     `json:"current_switchpoint"`          // Current program switchpoint
	PSActive                 bool    `json:"ps_active"`                    // Power save active
	PowersaveMode            bool    `json:"powersave_mode"`               // Powersave mode enabled
	FPActive                 bool    `json:"fp_active"`                    // Fireplace mode active
	FireplaceMode            bool    `json:"fireplace_mode"`               // Fireplace mode enabled
	TempOverride             bool    `json:"temp_override"`                // Temperature override active
	HolidayMode              bool    `json:"holiday_mode"`                 // Holiday mode active
	BoilerBlock              bool    `json:"boiler_block"`                 // Boiler blocked
	BoilerLock               bool    `json:"boiler_lock"`                  // Boiler locked
	BoilerMaintenance        bool    `json:"boiler_maintenance"`           // Maintenance required
	TempSetpoint             float64 `json:"temp_setpoint"`                // Current temperature setpoint
	TempOverrideTempSetpoint float64 `json:"temp_override_temp_setpoint"`  // Override temperature setpoint
	TempManualSetpoint       float64 `json:"temp_manual_setpoint"`         // Manual mode setpoint
	HEDEnabled               bool    `json:"hed_enabled"`                  // Home/Away detection enabled
	HEDDeviceAtHome          bool    `json:"hed_device_at_home"`           // Device detected at home
	OutdoorTemp              float64 `json:"outdoor_temp,omitempty"`       // Outdoor temperature (if requested)
	OutdoorSourceType        string  `json:"outdoor_source_type,omitempty"` // Source of outdoor temp data
}

// Pressure contains system pressure readings and valid operating ranges.
type Pressure struct {
	Pressure float64 `json:"pressure"`
	Unit     string  `json:"unit"`              // e.g., "bar"
	MinValue float64 `json:"min_value"`
	MaxValue float64 `json:"max_value"`
}

// HotWaterSupply contains hot water system operational status.
type HotWaterSupply struct {
	Active bool   `json:"active"`
	Mode   string `json:"mode"`
}

// Location contains device geographic position and timezone.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
}

// ProgramSwitchpoint defines a scheduled temperature change at a specific time and day.
type ProgramSwitchpoint struct {
	DayOfWeek   int     `json:"day_of_week"`    // 0=Sunday, 1=Monday, etc.
	Time        string  `json:"time"`           // HH:MM format
	Temperature float64 `json:"temperature"`
}

// Program defines a heating schedule with multiple temperature switchpoints.
type Program struct {
	Active      bool                  `json:"active"`
	Switchpoints []ProgramSwitchpoint `json:"switchpoints"`
}

// GasUsage contains cumulative gas consumption readings.
type GasUsage struct {
	Day   float64 `json:"day"`
	Month float64 `json:"month"`
	Year  float64 `json:"year"`
	Unit  string  `json:"unit"`   // e.g., "m³"
}

// SetTemperatureResult contains the outcome of a temperature setpoint change.
type SetTemperatureResult struct {
	Status             string  `json:"status"`               // "ok" or error message
	NewSetpoint        float64 `json:"new_setpoint"`
	PreviousSetpoint   float64 `json:"previous_setpoint"`
	CurrentTemperature float64 `json:"current_temperature"`
}

// RawResponse wraps generic API responses for endpoints without specific types.
type RawResponse struct {
	Value   interface{} `json:"value"`
	Type    string      `json:"type,omitempty"`
	UnitOfMeasure string `json:"unitOfMeasure,omitempty"`
	MinValue interface{} `json:"minValue,omitempty"`
	MaxValue interface{} `json:"maxValue,omitempty"`
	SrcType  string      `json:"srcType,omitempty"`
}
