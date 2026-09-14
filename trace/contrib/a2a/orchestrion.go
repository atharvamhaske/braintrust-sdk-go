// Package tracea2a provides automatic instrumentation for the A2A Go SDK.
//
// This file keeps packages referenced by orchestrion.yml in the module graph.
package tracea2a

import (
	// Dependencies used by orchestrion.yml template.
	_ "github.com/a2aproject/a2a-go/a2aclient"
	// Dependencies used by orchestrion.yml template.
	_ "github.com/a2aproject/a2a-go/a2asrv"
)
