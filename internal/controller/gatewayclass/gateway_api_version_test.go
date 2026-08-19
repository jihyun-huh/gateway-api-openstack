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

package gatewayclass

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"
)

func TestObserveGatewayAPIVersionReturnsDeterministicIssues(t *testing.T) {
	definitions := []client.Object{
		gatewayAPICRD("httproutes.gateway.networking.k8s.io", gatewayconsts.BundleVersion),
		gatewayAPICRD("gateways.gateway.networking.k8s.io", ""),
		gatewayAPICRD("gatewayclasses.gateway.networking.k8s.io", "v1.5.0"),
	}
	otherGroup := gatewayAPICRD("widgets.example.com", "")
	otherGroup.Spec.Group = "example.com"
	nestedGroup := gatewayAPICRD("widgets.foo.gateway.networking.k8s.io", "")
	nestedGroup.Spec.Group = "foo.gateway.networking.k8s.io"
	definitions = append(definitions, otherGroup, nestedGroup)
	baseReader := indexedFakeClientBuilder(testScheme(t), testConfig()).WithObjects(definitions...).Build()
	reader := &metadataOnlyCRDReader{Reader: baseReader}

	observation, err := ObserveGatewayAPIVersion(context.Background(), reader)
	if err != nil {
		t.Fatalf("ObserveGatewayAPIVersion() error = %v", err)
	}
	if observation.Supported {
		t.Fatal("ObserveGatewayAPIVersion() reported mixed CRDs as supported")
	}
	want := fmt.Sprintf(
		"Installed Gateway API CRDs do not match supported bundle version %s: "+
			"gatewayclasses.gateway.networking.k8s.io uses bundle version v1.5.0, "+
			"gateways.gateway.networking.k8s.io has no bundle version annotation",
		gatewayconsts.BundleVersion,
	)
	if observation.Message != want {
		t.Fatalf("observation message = %q, want %q", observation.Message, want)
	}
	if reader.listCalls != 1 {
		t.Fatalf("metadata List calls = %d, want 1", reader.listCalls)
	}
}

type metadataOnlyCRDReader struct {
	client.Reader
	listCalls int
}

func (r *metadataOnlyCRDReader) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	if _, ok := list.(*metav1.PartialObjectMetadataList); !ok {
		return fmt.Errorf("expected PartialObjectMetadataList, got %T", list)
	}
	r.listCalls++
	return r.Reader.List(ctx, list, opts...)
}
