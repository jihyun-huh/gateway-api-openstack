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

package gateway

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/controller"
)

func (r *Reconciler) bindGateway(
	ctx context.Context,
	expected *gatewayv1.Gateway,
	desiredListenerPort string,
) error {
	snapshot, err := r.observeGatewayMutationSnapshot(
		ctx,
		expected,
		gatewayBindingOptional,
		desiredListenerPort,
	)
	if err != nil {
		return err
	}
	gateway := snapshot.gateway.DeepCopy()

	metadataBase := gateway.DeepCopy()
	if gateway.Annotations == nil {
		gateway.Annotations = map[string]string{}
	}
	gateway.Annotations[r.Config.GatewayListenerPortAnnotation()] = desiredListenerPort
	gateway.Annotations[r.Config.GatewayClusterIDAnnotation()] = r.Config.ClusterID
	gateway.Annotations[r.Config.GatewayProjectIDAnnotation()] = r.Config.OpenStackProjectID
	controllerutil.AddFinalizer(gateway, r.Config.GatewayFinalizer())
	if equality.Semantic.DeepEqual(metadataBase.ObjectMeta, gateway.ObjectMeta) {
		return nil
	}
	if err := r.sealGatewayMutationSnapshot(ctx, snapshot); err != nil {
		return err
	}
	if err := r.Patch(ctx, gateway, controller.OptimisticMergeFrom(metadataBase)); err != nil {
		return fmt.Errorf("patch Gateway resource binding: %w", err)
	}
	return nil
}
