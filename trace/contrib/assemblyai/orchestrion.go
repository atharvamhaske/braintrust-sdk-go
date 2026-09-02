// Package assemblyai provides automatic instrumentation for
// github.com/AssemblyAI/assemblyai-go-sdk.
//
// This file ensures dependencies are in the module graph for orchestrion.
// When using orchestrion, the code generated from orchestrion.yml needs these
// packages available at compile time.
package assemblyai

import (
	// Dependencies used by orchestrion.yml template
	_ "github.com/AssemblyAI/assemblyai-go-sdk"
)
