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

// Package cloudauth validates and authenticates the exact clouds.yaml bytes
// used by a run-scoped E2E process.
package cloudauth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	openstackclouds "github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"sigs.k8s.io/yaml"

	"github.com/jihyun-huh/gateway-api-openstack/internal/cloud/openstack"
)

// ProjectIDResolver authenticates one clouds.yaml file and returns the project
// ID carried by the resulting token. The path points to an isolated exact copy
// that exists only for the duration of the call.
type ProjectIDResolver func(ctx context.Context, cloudsYAMLPath, cloudName, region, microversion string) (string, error)

// Options selects the cloud and authentication implementation.
type Options struct {
	CloudName        string
	Region           string
	Microversion     string
	ResolveProjectID ProjectIDResolver
}

// Validate verifies that contents contain exactly one self-contained cloud
// using TLS verification and an application credential. It rejects references
// to auxiliary TLS files because the quick E2E runner copies only clouds.yaml.
func Validate(contents []byte, cloudName string) error {
	cloud, err := selectCloud(contents, cloudName)
	if err != nil {
		return err
	}
	return validateCloud(cloud)
}

func validateCloud(cloud openstackclouds.Cloud) error {
	if cloud.Profile != "" || cloud.Cloud != "" {
		return fmt.Errorf("clouds.yaml must contain one self-contained verified cloud entry")
	}
	if cloud.Verify != nil && !*cloud.Verify {
		return fmt.Errorf("clouds.yaml must contain one self-contained verified cloud entry")
	}
	if cloud.CACertFile != "" || cloud.ClientCertFile != "" || cloud.ClientKeyFile != "" {
		return fmt.Errorf("clouds.yaml must not reference auxiliary TLS files")
	}
	if cloud.AuthType != "" && cloud.AuthType != openstackclouds.AuthV3ApplicationCredential {
		return fmt.Errorf("clouds.yaml must use an application credential")
	}
	return validateApplicationCredential(*cloud.AuthInfo)
}

func selectCloud(contents []byte, cloudName string) (openstackclouds.Cloud, error) {
	if len(contents) == 0 {
		return openstackclouds.Cloud{}, fmt.Errorf("clouds.yaml configuration is incomplete")
	}
	if cloudName == "" || strings.TrimSpace(cloudName) != cloudName {
		return openstackclouds.Cloud{}, fmt.Errorf("clouds.yaml configuration is incomplete")
	}
	var document openstackclouds.Clouds
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return openstackclouds.Cloud{}, fmt.Errorf("clouds.yaml could not be parsed")
	}
	cloud, found := document.Clouds[cloudName]
	if len(document.Clouds) != 1 || !found || cloud.AuthInfo == nil {
		return openstackclouds.Cloud{}, fmt.Errorf("clouds.yaml must contain one self-contained verified cloud entry")
	}
	return cloud, nil
}

// AuthenticateProject validates contents, authenticates an isolated exact
// copy, and returns the project ID from the issued token.
func AuthenticateProject(ctx context.Context, contents []byte, options Options) (string, error) {
	var projectID string
	err := withAuthenticatedExactCopy(ctx, contents, options, func(_ string, authenticatedProjectID string) error {
		projectID = authenticatedProjectID
		return nil
	})
	return projectID, err
}

func withAuthenticatedExactCopy(
	ctx context.Context,
	contents []byte,
	options Options,
	use func(path, projectID string) error,
) error {
	if err := Validate(contents, options.CloudName); err != nil {
		return err
	}
	if err := validateOptions(options); err != nil {
		return err
	}
	path, cleanup, err := writeExactCloudsYAML(contents)
	if err != nil {
		return err
	}
	defer cleanup()

	resolver := options.ResolveProjectID
	if resolver == nil {
		resolver = resolveProjectID
	}
	projectID, err := resolver(ctx, path, options.CloudName, options.Region, options.Microversion)
	if err != nil || projectID == "" || strings.TrimSpace(projectID) != projectID {
		return fmt.Errorf("openstack credential authentication failed")
	}
	return use(path, projectID)
}

// VerifyProject requires the authenticated project to match the explicitly
// approved project without including either project ID in an error.
func VerifyProject(ctx context.Context, contents []byte, expectedProjectID string, options Options) error {
	return VerifyProjectWithExactCopy(ctx, contents, expectedProjectID, options, func(string) error { return nil })
}

// VerifyProjectWithExactCopy verifies the approved project and invokes use
// while the same private clouds.yaml copy used for authentication still exists.
func VerifyProjectWithExactCopy(
	ctx context.Context,
	contents []byte,
	expectedProjectID string,
	options Options,
	use func(path string) error,
) error {
	if expectedProjectID == "" || strings.TrimSpace(expectedProjectID) != expectedProjectID {
		return fmt.Errorf("expected OpenStack project must be explicit")
	}
	if use == nil {
		return fmt.Errorf("verified OpenStack credential callback must not be nil")
	}
	return withAuthenticatedExactCopy(ctx, contents, options, func(path, projectID string) error {
		if projectID != expectedProjectID {
			return fmt.Errorf("openstack credential did not authenticate to the approved project")
		}
		return use(path)
	})
}

func validateApplicationCredential(auth openstackclouds.AuthInfo) error {
	if auth.AuthURL == "" {
		return fmt.Errorf("clouds.yaml does not satisfy the application credential contract")
	}
	if credentialIdentityCount(auth) != 1 {
		return fmt.Errorf("clouds.yaml does not satisfy the application credential contract")
	}
	if auth.ApplicationCredentialSecret == "" || hasConflictingAuthentication(auth) {
		return fmt.Errorf("clouds.yaml does not satisfy the application credential contract")
	}
	if auth.ApplicationCredentialName != "" && auth.UserID == "" && auth.Username == "" {
		return fmt.Errorf("named application credential does not identify its user")
	}
	return nil
}

func validateOptions(options Options) error {
	if options.Region == "" || strings.TrimSpace(options.Region) != options.Region {
		return fmt.Errorf("controller cloud region and microversion must be explicit")
	}
	if options.Microversion == "" || strings.TrimSpace(options.Microversion) != options.Microversion {
		return fmt.Errorf("controller cloud region and microversion must be explicit")
	}
	return nil
}

func credentialIdentityCount(auth openstackclouds.AuthInfo) int {
	identities := 0
	for _, value := range []string{auth.ApplicationCredentialID, auth.ApplicationCredentialName} {
		if value != "" {
			identities++
		}
	}
	return identities
}

func hasConflictingAuthentication(auth openstackclouds.AuthInfo) bool {
	for _, value := range []string{auth.Password, auth.Token, auth.SystemScope, auth.TrustID} {
		if value != "" {
			return true
		}
	}
	return false
}

func writeExactCloudsYAML(contents []byte) (string, func(), error) {
	directory, err := os.MkdirTemp("", "gateway-api-openstack-e2e-auth-")
	if err != nil {
		return "", func() {}, fmt.Errorf("prepare controller credential verification")
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, "clouds.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("prepare controller credential verification")
	}
	return path, cleanup, nil
}

func resolveProjectID(ctx context.Context, cloudsYAMLPath, cloudName, region, microversion string) (string, error) {
	clients, err := openstack.NewServiceClients(ctx, openstack.ClientConfig{
		Region:         region,
		Microversion:   microversion,
		CloudsYAMLPath: cloudsYAMLPath,
		CloudName:      cloudName,
		APIQPS:         openstack.DefaultAPIQPS,
		APIBurst:       openstack.DefaultAPIBurst,
	})
	if err != nil {
		return "", err
	}
	return clients.ProjectID, nil
}
