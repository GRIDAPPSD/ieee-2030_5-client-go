package inverter

import "time"

// Scenario defines a test scenario with a timeline of grid state changes.
type Scenario struct {
	Name        string
	Description string
	Duration    time.Duration
	Steps       []ScenarioStep
}

// ScenarioStep applies grid state overrides or server control changes at a given time.
type ScenarioStep struct {
	AtTime      time.Duration
	Grid        *GridState // nil = no change
	Description string
}

// AllScenarios returns all available test scenarios.
func AllScenarios() map[string]Scenario {
	return map[string]Scenario{
		"normal":       NormalScenario(),
		"voltvar":      VoltVarScenario(),
		"powerlimit":   PowerLimitScenario(),
		"disconnect":   DisconnectScenario(),
		"freqdroop":    FreqDroopScenario(),
		"voltageride":  VoltageRideScenario(),
		"enterservice": EnterServiceScenario(),
		"lifecycle":    LifecycleScenario(),
	}
}

// NormalScenario simulates a sunny day with stable grid conditions.
func NormalScenario() Scenario {
	return Scenario{
		Name:        "normal",
		Description: "Sunny day, stable grid, PF=1.0, periodic metering",
		Duration:    24 * time.Hour,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "stable grid"},
		},
	}
}

// VoltVarScenario simulates voltage rise triggering volt-var response.
func VoltVarScenario() Scenario {
	return Scenario{
		Name:        "voltvar",
		Description: "Voltage rises to 1.05-1.08 p.u., triggering var absorption",
		Duration:    1 * time.Hour,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "normal voltage"},
			{AtTime: 10 * time.Minute, Grid: &GridState{VoltsPU: 1.03, FreqHz: 60.0}, Description: "voltage rising"},
			{AtTime: 20 * time.Minute, Grid: &GridState{VoltsPU: 1.05, FreqHz: 60.0}, Description: "voltage at 1.05 p.u. — absorbing vars"},
			{AtTime: 30 * time.Minute, Grid: &GridState{VoltsPU: 1.08, FreqHz: 60.0}, Description: "voltage at 1.08 p.u. — full var absorption"},
			{AtTime: 40 * time.Minute, Grid: &GridState{VoltsPU: 1.03, FreqHz: 60.0}, Description: "voltage decreasing"},
			{AtTime: 50 * time.Minute, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "voltage normal"},
		},
	}
}

// PowerLimitScenario simulates a server-commanded active power limit.
func PowerLimitScenario() Scenario {
	return Scenario{
		Name:        "powerlimit",
		Description: "Server sends opModMaxLimW=5000W, inverter curtails",
		Duration:    30 * time.Minute,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "normal operation at 10kW"},
			{AtTime: 5 * time.Minute, Description: "server sends maxLimW=5000 (apply via DER control poll)"},
			{AtTime: 20 * time.Minute, Description: "server removes limit"},
		},
	}
}

// DisconnectScenario simulates a server-commanded disconnect and reconnect.
func DisconnectScenario() Scenario {
	return Scenario{
		Name:        "disconnect",
		Description: "Server sends opModConnect=false, then re-enables",
		Duration:    30 * time.Minute,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "normal operation"},
			{AtTime: 5 * time.Minute, Description: "server sends connect=false — cease to energize"},
			{AtTime: 15 * time.Minute, Description: "server sends connect=true — reconnect"},
		},
	}
}

// FreqDroopScenario simulates frequency deviation triggering droop response.
func FreqDroopScenario() Scenario {
	return Scenario{
		Name:        "freqdroop",
		Description: "Frequency drops below nominal, inverter adjusts power via droop",
		Duration:    30 * time.Minute,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "normal frequency"},
			{AtTime: 5 * time.Minute, Grid: &GridState{VoltsPU: 1.0, FreqHz: 59.5}, Description: "frequency dropping"},
			{AtTime: 10 * time.Minute, Grid: &GridState{VoltsPU: 1.0, FreqHz: 58.5}, Description: "frequency at 58.5 Hz — droop active"},
			{AtTime: 20 * time.Minute, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "frequency recovered"},
		},
	}
}

// VoltageRideScenario simulates voltage sags with ride-through and trip.
func VoltageRideScenario() Scenario {
	return Scenario{
		Name:        "voltageride",
		Description: "Voltage sag to 0.7 p.u. (ride-through), then 0.45 p.u. (trip)",
		Duration:    15 * time.Minute,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "normal"},
			{AtTime: 2 * time.Minute, Grid: &GridState{VoltsPU: 0.70, FreqHz: 60.0}, Description: "voltage sag — ride through"},
			{AtTime: 2*time.Minute + 5*time.Second, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "voltage recovered"},
			{AtTime: 5 * time.Minute, Grid: &GridState{VoltsPU: 0.45, FreqHz: 60.0}, Description: "deep sag — must trip"},
			{AtTime: 5*time.Minute + 200*time.Millisecond, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "voltage restored (wait 300s to reconnect)"},
		},
	}
}

// EnterServiceScenario simulates startup with abnormal conditions.
func EnterServiceScenario() Scenario {
	return Scenario{
		Name:        "enterservice",
		Description: "Inverter starts with low voltage, waits for Table 4 criteria",
		Duration:    15 * time.Minute,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 0.85, FreqHz: 60.0}, Description: "low voltage — cannot enter service"},
			{AtTime: 3 * time.Minute, Grid: &GridState{VoltsPU: 0.95, FreqHz: 60.0}, Description: "voltage normal — start 300s timer"},
			{AtTime: 8*time.Minute + 1*time.Second, Description: "300s elapsed — enter service"},
		},
	}
}

// LifecycleScenario exercises the complete protocol flow.
func LifecycleScenario() Scenario {
	return Scenario{
		Name:        "lifecycle",
		Description: "Full lifecycle: register → setup → control → metering → disconnect → reconnect",
		Duration:    1 * time.Hour,
		Steps: []ScenarioStep{
			{AtTime: 0, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "startup and registration"},
			{AtTime: 5 * time.Minute, Description: "DER setup (PUT capability/settings)"},
			{AtTime: 10 * time.Minute, Grid: &GridState{VoltsPU: 1.05, FreqHz: 60.0}, Description: "voltage rise — volt-var active"},
			{AtTime: 20 * time.Minute, Grid: &GridState{VoltsPU: 1.0, FreqHz: 60.0}, Description: "voltage normal"},
			{AtTime: 25 * time.Minute, Description: "power limit to 5kW"},
			{AtTime: 35 * time.Minute, Description: "remove power limit"},
			{AtTime: 40 * time.Minute, Description: "disconnect command"},
			{AtTime: 50 * time.Minute, Description: "reconnect command"},
		},
	}
}
