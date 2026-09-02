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

package e2e

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/controllercontract"
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

const (
	enableEnvironment        = runconfig.EnableEnvironment
	runtimeConfigEnvironment = runconfig.RuntimeConfigEnvironment
	runAnnotation            = controllercontract.RunAnnotation
)

type environmentReader func(string) string

type e2eConfig = runconfig.Runtime
type projectMode = runconfig.ProjectMode

const (
	projectModeDedicated = runconfig.ProjectModeDedicated
	projectModeShared    = runconfig.ProjectModeShared
)

func loadE2EConfig(getenv environmentReader) (e2eConfig, bool, error) {
	if getenv(enableEnvironment) != "true" {
		return e2eConfig{}, false, nil
	}

	path := strings.TrimSpace(getenv(runtimeConfigEnvironment))
	if path == "" || !filepath.IsAbs(path) {
		return e2eConfig{}, true, fmt.Errorf("%s must be an absolute path", runtimeConfigEnvironment)
	}
	config, err := runconfig.LoadRuntime(path)
	if err != nil {
		return e2eConfig{}, true, fmt.Errorf("load private E2E runtime configuration: %w", err)
	}
	return config, true, nil
}
