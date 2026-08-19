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
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayconsts "sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

type Config = controller.Config

const gatewayAPIExperimentalGroup = "gateway.networking.x-k8s.io"

var ObserveGatewayAPIVersion = controller.ObserveGatewayAPIVersion

var requiredGatewayAPICRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
}

func testConfig() Config {
	return Config{ControllerName: "example.com/gateway-api-openstack"}
}

func rawIndexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *fake.ClientBuilder {
	builder := fake.NewClientBuilder().WithScheme(scheme)
	indexer := &fakeFieldIndexer{builder: builder, scheme: scheme}
	if err := controller.SetupIndexes(context.Background(), indexer, config); err != nil {
		panic(err)
	}
	return builder
}

func indexedFakeClientBuilder(scheme *runtime.Scheme, config Config) *fake.ClientBuilder {
	return rawIndexedFakeClientBuilder(scheme, config)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func unsupportedGatewayAPICRDs() []client.Object {
	definitions := supportedGatewayAPICRDs()
	for _, definition := range definitions {
		if definition.GetName() == "httproutes.gateway.networking.k8s.io" {
			definition.SetAnnotations(map[string]string{gatewayconsts.BundleVersionAnnotation: "v1.5.0"})
		}
	}
	return definitions
}

type fakeFieldIndexer struct {
	builder *fake.ClientBuilder
	scheme  *runtime.Scheme
}

func (i *fakeFieldIndexer) IndexField(
	_ context.Context,
	object client.Object,
	field string,
	extract client.IndexerFunc,
) error {
	if _, _, err := i.scheme.ObjectKinds(object); err != nil {
		return nil
	}
	i.builder.WithIndex(object, field, extract)
	return nil
}
