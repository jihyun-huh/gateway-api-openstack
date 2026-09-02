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

package main

import (
	"io"
	"time"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/runconfig"
)

const (
	controllerConfigMapName  = "openstack-gateway-controller-config"
	controllerServiceAccount = "openstack-gateway-controller"
	controllerCloudsVolume   = "openstack-clouds"
)

type fileConfig = runconfig.File
type resolvedConfig = runconfig.Runtime

type resolveOptions struct {
	repositoryRoot string
	now            func() time.Time
	random         io.Reader
}

func loadFileConfig(path string) (fileConfig, error) {
	return runconfig.Load(path)
}

func resolveFileConfig(config fileConfig, options resolveOptions) (resolvedConfig, error) {
	return runconfig.Resolve(config, runconfig.ResolveOptions{
		RepositoryRoot: options.repositoryRoot,
		Now:            options.now,
		Random:         options.random,
	})
}
