package types

const (
	// Status endpoints
	URIStatus      = "/ecus/rrc/uiStatus"
	URIOutdoorTemp = "/system/sensors/temperatures/outdoor_t1"

	// Pressure endpoints
	URIPressure = "/system/appliance/systemPressure"

	// Hot water endpoints
	URIHotWaterClockMode  = "/dhwCircuits/dhwA/dhwOperationClockMode"
	URIHotWaterManualMode = "/dhwCircuits/dhwA/dhwOperationManualMode"

	// User mode endpoints
	// URIUserMode controls the heating operation mode.
	// Valid values for PUT requests:
	//   - "manual": Manual mode - user controls temperature directly
	//   - "clock": Clock/scheduled mode - follows programmed heating schedule
	// Note: "off" is NOT a valid mode value and will result in HTTP 400 Bad Request.
	// To effectively disable heating, use manual mode with a low temperature setpoint.
	URIUserMode = "/heatingCircuits/hc1/usermode"

	// Temperature control endpoints
	URIManualSetpoint           = "/heatingCircuits/hc1/temperatureRoomManual"
	URIManualTempOverrideStatus = "/heatingCircuits/hc1/manualTempOverride/status"
	URIManualTempOverrideTemp   = "/heatingCircuits/hc1/manualTempOverride/temperature"

	// Program endpoints
	URIActiveProgram = "/ecus/rrc/userprogram/activeprogram"
	URIProgram1      = "/ecus/rrc/userprogram/program1"
	URIProgram2      = "/ecus/rrc/userprogram/program2"

	// Location endpoints
	URILocationLatitude  = "/system/location/latitude"
	URILocationLongitude = "/system/location/longitude"

	// Display code endpoints
	URIDisplayCode = "/system/appliance/displaycode"
	URICauseCode   = "/system/appliance/causecode"

	// Gas usage endpoint
	URIGasUsage = "/ecus/rrc/recordings/gasusage"

	// Fireplace mode endpoint
	URIFireplaceMode = "/ecus/rrc/userprogram/fireplacefunction"

	// Supply temperature endpoint
	URISupplyTemp = "/heatingCircuits/hc1/actualSupplyTemperature"
)
