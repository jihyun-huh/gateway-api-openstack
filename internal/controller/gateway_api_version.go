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

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

var requiredGatewayAPICRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
}

const (
	// UnsupportedGatewayAPIVersionMessage is the stable status message used
	// when the installed Gateway API CRDs do not match the supported bundle.
	UnsupportedGatewayAPIVersionMessage = "GatewayClass does not support the installed Gateway API CRD version"
	gatewayAPIExperimentalGroup         = "gateway.networking.x-k8s.io"
	gatewayAPIVersionRequeue            = time.Minute
)

// GatewayAPIVersionRequeueAfter returns the stable, jittered retry interval for
// checking the installed Gateway API bundle again.
func GatewayAPIVersionRequeueAfter(uid types.UID) time.Duration {
	return stableJitter(gatewayAPIVersionRequeue, string(uid)+"/gateway-api-version")
}

// ErrUnsupportedGatewayAPIVersion indicates that the installed Gateway API
// CRDs do not match the bundle supported by this controller.
var ErrUnsupportedGatewayAPIVersion = errors.New("installed Gateway API CRD version is unsupported")

// GatewayAPIVersionObservation describes the compatibility of the installed
// Gateway API CRD bundle.
type GatewayAPIVersionObservation struct {
	// Supported reports whether every required CRD uses the supported bundle.
	Supported bool
	// Message summarizes the observed bundle state for status reporting.
	Message string
}

// ObserveGatewayAPIVersion reads Gateway API CRD metadata and evaluates the
// installed bundle version.
func ObserveGatewayAPIVersion(ctx context.Context, reader client.Reader) (GatewayAPIVersionObservation, error) {
	var definitions metav1.PartialObjectMetadataList
	definitions.SetGroupVersionKind(
		apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinitionList"),
	)
	if err := reader.List(ctx, &definitions); err != nil {
		return GatewayAPIVersionObservation{}, fmt.Errorf("list Gateway API custom resource definitions: %w", err)
	}

	present := make(map[string]struct{})
	issues := make([]string, 0)
	for _, definition := range definitions.Items {
		if !IsGatewayAPICRDName(definition.Name) {
			continue
		}
		present[definition.Name] = struct{}{}
		if !definition.DeletionTimestamp.IsZero() {
			issues = append(issues, fmt.Sprintf("%s is being deleted", definition.Name))
			continue
		}
		installedVersion := definition.Annotations[gatewayconsts.BundleVersionAnnotation]
		switch {
		case installedVersion == "":
			issues = append(issues, fmt.Sprintf("%s has no bundle version annotation", definition.Name))
		case installedVersion != gatewayconsts.BundleVersion:
			issues = append(issues, fmt.Sprintf("%s uses bundle version %s", definition.Name, installedVersion))
		}
	}
	for _, name := range requiredGatewayAPICRDs {
		if _, found := present[name]; !found {
			issues = append(issues, fmt.Sprintf("%s is not installed", name))
		}
	}

	if len(issues) == 0 {
		return GatewayAPIVersionObservation{
			Supported: true,
			Message:   fmt.Sprintf("Installed Gateway API CRDs use supported bundle version %s", gatewayconsts.BundleVersion),
		}, nil
	}
	sort.Strings(issues)
	return GatewayAPIVersionObservation{
		Message: fmt.Sprintf(
			"Installed Gateway API CRDs do not match supported bundle version %s: %s",
			gatewayconsts.BundleVersion,
			strings.Join(issues, ", "),
		),
	}, nil
}

// IsGatewayAPICRDName reports whether a CRD name belongs to a Gateway API
// group.
func IsGatewayAPICRDName(name string) bool {
	resource, group, found := strings.Cut(name, ".")
	return found && resource != "" && isGatewayAPIGroup(group)
}

func isGatewayAPIGroup(group string) bool {
	return group == gatewayv1.GroupName || group == gatewayAPIExperimentalGroup
}

// GatewayClassSupportsInstalledVersion reports whether the GatewayClass has a
// current SupportedVersion=True condition.
func GatewayClassSupportsInstalledVersion(gatewayClass *gatewayv1.GatewayClass) bool {
	condition := meta.FindStatusCondition(
		gatewayClass.Status.Conditions,
		string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
	)
	return condition != nil && condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == gatewayClass.Generation
}

// ValidateInstalledGatewayAPIVersion returns an error unless a live read shows
// a supported Gateway API CRD bundle.
func ValidateInstalledGatewayAPIVersion(ctx context.Context, reader client.Reader) error {
	if reader == nil {
		return ErrAPIReaderRequired
	}
	observation, err := ObserveGatewayAPIVersion(ctx, reader)
	if err != nil {
		return err
	}
	if !observation.Supported {
		return ErrUnsupportedGatewayAPIVersion
	}
	return nil
}
