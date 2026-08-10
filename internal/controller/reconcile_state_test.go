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
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconcilersKeepRequestStateLocal(t *testing.T) {
	t.Helper()

	loggerType := reflect.TypeOf(logr.Logger{})
	requestType := reflect.TypeOf(ctrl.Request{})
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	objectType := reflect.TypeOf((*client.Object)(nil)).Elem()

	for name, reconciler := range map[string]any{
		"GatewayClassReconciler": GatewayClassReconciler{},
		"GatewayReconciler":      GatewayReconciler{},
		"HTTPRouteReconciler":    HTTPRouteReconciler{},
	} {
		reconcilerType := reflect.TypeOf(reconciler)
		for index := range reconcilerType.NumField() {
			field := reconcilerType.Field(index)
			switch {
			case field.Type == loggerType:
				t.Errorf("%s.%s stores a request logger", name, field.Name)
			case field.Type == requestType:
				t.Errorf("%s.%s stores a reconcile request", name, field.Name)
			case field.Type.Implements(contextType):
				t.Errorf("%s.%s stores a context", name, field.Name)
			case field.Type.Implements(objectType):
				t.Errorf("%s.%s stores a Kubernetes object", name, field.Name)
			}
		}
	}
}
