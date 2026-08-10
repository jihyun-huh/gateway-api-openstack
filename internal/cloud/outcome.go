/*
Copyright 2026 The gateway-api-openstack Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloud

import (
	"fmt"
	"time"
)

// OutcomeState describes whether an ensure operation has converged or still
// needs observation. Failures are returned separately as errors.
type OutcomeState string

const (
	// OutcomeReady means the provider observed the requested state.
	OutcomeReady OutcomeState = "Ready"
	// OutcomeProgressing means an asynchronous provider operation is not ready
	// for another mutation and should be observed again later.
	OutcomeProgressing OutcomeState = "Progressing"
)

// Outcome describes the progress of a provider ensure operation.
type Outcome struct {
	State        OutcomeState
	Message      string
	RequeueAfter time.Duration
}

// Validate rejects an outcome that does not describe a supported provider
// state. The controller, rather than the provider, bounds RequeueAfter.
func (o Outcome) Validate() error {
	switch o.State {
	case OutcomeReady:
		if o.RequeueAfter != 0 {
			return fmt.Errorf("ready provider outcome has a requeue interval")
		}
		return nil
	case OutcomeProgressing:
		return nil
	default:
		return fmt.Errorf("provider returned unknown outcome state %q", o.State)
	}
}

// ReadyOutcome returns an outcome for converged provider state.
func ReadyOutcome() Outcome {
	return Outcome{State: OutcomeReady}
}

// ProgressingOutcome returns an outcome that asks the controller to observe
// the provider again after requeueAfter.
func ProgressingOutcome(message string, requeueAfter time.Duration) Outcome {
	return Outcome{
		State:        OutcomeProgressing,
		Message:      message,
		RequeueAfter: requeueAfter,
	}
}

// GatewayResult is the provider-neutral result of ensuring a Gateway graph.
type GatewayResult struct {
	State   GatewayState
	Outcome Outcome
}

// GatewayReadyResult returns a converged Gateway result.
func GatewayReadyResult(state GatewayState) GatewayResult {
	return GatewayResult{State: state, Outcome: ReadyOutcome()}
}

// GatewayProgressingResult returns a Gateway result that needs another
// observation.
func GatewayProgressingResult(message string, requeueAfter time.Duration) GatewayResult {
	return GatewayResult{Outcome: ProgressingOutcome(message, requeueAfter)}
}

// RouteResult is the provider-neutral result of ensuring an HTTPRoute graph.
type RouteResult struct {
	State   RouteState
	Outcome Outcome
}

// RouteReadyResult returns a converged HTTPRoute result.
func RouteReadyResult(state RouteState) RouteResult {
	return RouteResult{State: state, Outcome: ReadyOutcome()}
}

// RouteProgressingResult returns an HTTPRoute result that needs another
// observation.
func RouteProgressingResult(message string, requeueAfter time.Duration) RouteResult {
	return RouteResult{Outcome: ProgressingOutcome(message, requeueAfter)}
}
