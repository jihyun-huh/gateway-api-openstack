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

package cloudauth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

const credentialSecretSentinel = "credential-secret-sentinel"

func TestValidateAcceptsSelfContainedApplicationCredential(t *testing.T) {
	for _, identity := range []string{
		"      application_credential_id: credential-id\n",
		"      application_credential_name: credential-name\n      username: e2e-user\n",
	} {
		contents := validCloudsYAML(identity)
		if err := Validate(contents, "openstack-e2e"); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
	}
}

func TestValidateRejectsUnsafeCloudsYAMLWithoutLeakingCredential(t *testing.T) {
	valid := string(validCloudsYAML("      application_credential_id: credential-id\n"))
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{name: "multiple entries", mutate: func(value string) string { return value + "  another: {}\n" }},
		{name: "wrong selected entry", mutate: func(value string) string { return value }},
		{name: "profile inheritance", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: true\n    profile: inherited\n", 1)
		}},
		{name: "cloud inheritance", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: true\n    cloud: inherited\n", 1)
		}},
		{name: "TLS verification disabled", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: false\n", 1)
		}},
		{name: "CA file", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: true\n    cacert: /secret/ca.crt\n", 1)
		}},
		{name: "client certificate", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: true\n    cert: /secret/client.crt\n", 1)
		}},
		{name: "client key", mutate: func(value string) string {
			return strings.Replace(value, "    verify: true\n", "    verify: true\n    key: /secret/client.key\n", 1)
		}},
		{name: "password authentication", mutate: func(value string) string {
			return strings.Replace(value, "auth_type: v3applicationcredential", "auth_type: password", 1)
		}},
		{name: "missing auth URL", mutate: func(value string) string {
			return strings.Replace(value, "      auth_url: https://keystone.example.test/v3\n", "", 1)
		}},
		{name: "two credential identities", mutate: func(value string) string {
			return strings.Replace(value, "      application_credential_id: credential-id\n", "      application_credential_id: credential-id\n      application_credential_name: other\n", 1)
		}},
		{name: "missing credential secret", mutate: func(value string) string {
			return strings.Replace(value, "      application_credential_secret: "+credentialSecretSentinel+"\n", "", 1)
		}},
		{name: "password field", mutate: func(value string) string {
			return strings.Replace(value, "      application_credential_secret: "+credentialSecretSentinel+"\n", "      application_credential_secret: "+credentialSecretSentinel+"\n      password: forbidden\n", 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cloudName := "openstack-e2e"
			if test.name == "wrong selected entry" {
				cloudName = "another"
			}
			err := Validate([]byte(test.mutate(valid)), cloudName)
			if err == nil || strings.Contains(err.Error(), credentialSecretSentinel) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestAuthenticateProjectUsesAndRemovesExactPrivateCopy(t *testing.T) {
	contents := validCloudsYAML("      application_credential_id: credential-id\n")
	var copiedPath string
	options := Options{
		CloudName:        "openstack-e2e",
		Region:           "RegionOne",
		Microversion:     "2.5",
		ResolveProjectID: exactCopyResolver(t, contents, &copiedPath),
	}
	projectID, err := AuthenticateProject(context.Background(), contents, options)
	if err != nil || projectID != "approved-project" {
		t.Fatalf("AuthenticateProject() = %q, %v", projectID, err)
	}
	if _, err := os.Stat(copiedPath); !os.IsNotExist(err) {
		t.Fatalf("temporary clouds.yaml remains: %v", err)
	}
}

func TestVerifyProjectWithExactCopyBindsCallbackAndCleansUp(t *testing.T) {
	contents := validCloudsYAML("      application_credential_id: credential-id\n")
	var authenticatedPath string
	var callbackPath string
	options := Options{
		CloudName:        "openstack-e2e",
		Region:           "RegionOne",
		Microversion:     "2.5",
		ResolveProjectID: exactCopyResolver(t, contents, &authenticatedPath),
	}
	err := VerifyProjectWithExactCopy(context.Background(), contents, "approved-project", options, func(path string) error {
		callbackPath = path
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, contents) {
			t.Fatal("callback did not receive the authenticated clouds.yaml bytes")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("VerifyProjectWithExactCopy() error = %v", err)
	}
	if callbackPath == "" || callbackPath != authenticatedPath {
		t.Fatalf("callback path = %q, authenticated path = %q", callbackPath, authenticatedPath)
	}
	if _, err := os.Stat(callbackPath); !os.IsNotExist(err) {
		t.Fatalf("temporary clouds.yaml remains: %v", err)
	}
}

func TestVerifyProjectWithExactCopyCleansUpAfterCallbackFailure(t *testing.T) {
	contents := validCloudsYAML("      application_credential_id: credential-id\n")
	var copiedPath string
	callbackErr := errors.New("audit callback failed")
	options := Options{
		CloudName:        "openstack-e2e",
		Region:           "RegionOne",
		Microversion:     "2.5",
		ResolveProjectID: exactCopyResolver(t, contents, &copiedPath),
	}
	err := VerifyProjectWithExactCopy(context.Background(), contents, "approved-project", options, func(string) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("VerifyProjectWithExactCopy() error = %v", err)
	}
	if _, err := os.Stat(copiedPath); !os.IsNotExist(err) {
		t.Fatalf("temporary clouds.yaml remains after callback failure: %v", err)
	}
}

func exactCopyResolver(t *testing.T, contents []byte, copiedPath *string) ProjectIDResolver {
	t.Helper()
	return func(_ context.Context, path, cloud, region, microversion string) (string, error) {
		*copiedPath = path
		if cloud != "openstack-e2e" || region != "RegionOne" || microversion != "2.5" {
			t.Fatalf("resolver received cloud = %q, region = %q, microversion = %q", cloud, region, microversion)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, contents) {
			t.Fatal("resolver did not receive an exact clouds.yaml copy")
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("clouds.yaml mode = %o, want 600", info.Mode().Perm())
		}
		return "approved-project", nil
	}
}

func TestVerifyProjectRedactsResolverErrorsAndProjectIDs(t *testing.T) {
	contents := validCloudsYAML("      application_credential_id: credential-id\n")
	options := Options{
		CloudName:    "openstack-e2e",
		Region:       "RegionOne",
		Microversion: "2.5",
		ResolveProjectID: func(context.Context, string, string, string, string) (string, error) {
			return "", errors.New(credentialSecretSentinel)
		},
	}
	err := VerifyProject(context.Background(), contents, "expected-project-sentinel", options)
	if err == nil || strings.Contains(err.Error(), credentialSecretSentinel) || strings.Contains(err.Error(), "expected-project-sentinel") {
		t.Fatalf("VerifyProject() error = %v", err)
	}
	options.ResolveProjectID = func(context.Context, string, string, string, string) (string, error) {
		return "actual-project-sentinel", nil
	}
	err = VerifyProject(context.Background(), contents, "expected-project-sentinel", options)
	if err == nil || strings.Contains(err.Error(), "actual-project-sentinel") || strings.Contains(err.Error(), "expected-project-sentinel") {
		t.Fatalf("VerifyProject() mismatch error = %v", err)
	}
}

func validCloudsYAML(identity string) []byte {
	return []byte("clouds:\n" +
		"  openstack-e2e:\n" +
		"    auth_type: v3applicationcredential\n" +
		"    auth:\n" +
		"      auth_url: https://keystone.example.test/v3\n" +
		identity +
		"      application_credential_secret: " + credentialSecretSentinel + "\n" +
		"    region_name: RegionOne\n" +
		"    verify: true\n")
}
