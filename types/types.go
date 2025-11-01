package types

// Status represents the complete system status
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

// Pressure represents system pressure information
type Pressure struct {
	Pressure float64 `json:"pressure"`          // Current pressure
	Unit     string  `json:"unit"`              // Unit of measurement (e.g., "bar")
	MinValue float64 `json:"min_value"`         // Minimum valid pressure
	MaxValue float64 `json:"max_value"`         // Maximum valid pressure
}

// HotWaterSupply represents hot water system status
type HotWaterSupply struct {
	Active bool   `json:"active"` // Is hot water active
	Mode   string `json:"mode"`   // Operation mode
}

// Location represents device location information
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timezone  string  `json:"timezone"`
}

// ProgramSwitchpoint represents a single program switchpoint
type ProgramSwitchpoint struct {
	DayOfWeek   int     `json:"day_of_week"`    // 0=Sunday, 1=Monday, etc.
	Time        string  `json:"time"`           // HH:MM format
	Temperature float64 `json:"temperature"`    // Target temperature
}

// Program represents the heating schedule
type Program struct {
	Active      bool                  `json:"active"`
	Switchpoints []ProgramSwitchpoint `json:"switchpoints"`
}

// GasUsage represents gas consumption data
type GasUsage struct {
	Day   float64 `json:"day"`    // Today's consumption
	Month float64 `json:"month"`  // This month's consumption
	Year  float64 `json:"year"`   // This year's consumption
	Unit  string  `json:"unit"`   // Unit (e.g., "m³")
}

// SetTemperatureResult represents the result of a temperature change
type SetTemperatureResult struct {
	Status             string  `json:"status"`               // "ok" or error message
	NewSetpoint        float64 `json:"new_setpoint"`         // New temperature setpoint
	PreviousSetpoint   float64 `json:"previous_setpoint"`    // Previous setpoint
	CurrentTemperature float64 `json:"current_temperature"`  // Current actual temperature
}

// RawResponse represents a raw API response for unknown endpoints
type RawResponse struct {
	Value   interface{} `json:"value"`
	Type    string      `json:"type,omitempty"`
	UnitOfMeasure string `json:"unitOfMeasure,omitempty"`
	MinValue interface{} `json:"minValue,omitempty"`
	MaxValue interface{} `json:"maxValue,omitempty"`
	SrcType  string      `json:"srcType,omitempty"`
}
