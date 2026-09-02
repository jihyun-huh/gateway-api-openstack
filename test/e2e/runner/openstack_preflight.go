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
	"context"
	"fmt"
	"os"

	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/auditexec"
	"github.com/jihyun-huh/gateway-api-openstack/test/e2e/internal/cloudauth"
)

type projectAuthenticator = cloudauth.ProjectIDResolver
type ownershipAuditRunner func(context.Context, resolvedConfig, []string) error

func preflightOpenStack(
	ctx context.Context,
	config resolvedConfig,
	controllerCloudsYAML []byte,
	authenticate projectAuthenticator,
	runAudit ownershipAuditRunner,
) error {
	options := cloudauth.Options{
		CloudName:        config.Audit.Cloud,
		Region:           config.Audit.Region,
		Microversion:     config.Audit.Microversion,
		ResolveProjectID: authenticate,
	}
	if err := cloudauth.VerifyProject(ctx, controllerCloudsYAML, config.Project.ExpectedProjectID, options); err != nil {
		return fmt.Errorf("verify controller OpenStack credential: %w", err)
	}
	auditCloudsYAML, err := os.ReadFile(config.Audit.CloudsYAML)
	if err != nil {
		return fmt.Errorf("read audit clouds.yaml: %w", err)
	}
	if runAudit == nil {
		runAudit = runEmptyOwnershipAudit
	}
	err = cloudauth.VerifyProjectWithExactCopy(
		ctx,
		auditCloudsYAML,
		config.Project.ExpectedProjectID,
		options,
		func(path string) error {
			auditConfig := config
			auditConfig.Audit.CloudsYAML = path
			if err := runAudit(ctx, auditConfig, mergeHarnessEnvironment(os.Environ(), nil)); err != nil {
				return fmt.Errorf("verify empty pre-install ownership scope: %w", err)
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("verify audit OpenStack credential and ownership scope: %w", err)
	}
	return nil
}

func runEmptyOwnershipAudit(ctx context.Context, config resolvedConfig, environment []string) error {
	_, err := auditexec.Run(ctx, auditexec.Config{
		Binary:         config.Audit.Binary,
		ControllerName: config.ControllerName,
		ClusterID:      config.Audit.ClusterID,
		Kubeconfig:     config.Kubeconfig,
		Context:        config.KubeContext,
		Microversion:   config.Audit.Microversion,
		CloudsYAML:     config.Audit.CloudsYAML,
		Cloud:          config.Audit.Cloud,
		Region:         config.Audit.Region,
		Environment:    environment,
	}, auditexec.Validation{RequireEmpty: true})
	return err
}
