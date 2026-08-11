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
	"sort"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func sortedRequests(keys map[types.NamespacedName]struct{}) []reconcile.Request {
	ordered := make([]types.NamespacedName, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Namespace != ordered[j].Namespace {
			return ordered[i].Namespace < ordered[j].Namespace
		}
		return ordered[i].Name < ordered[j].Name
	})

	requests := make([]reconcile.Request, 0, len(ordered))
	for _, key := range ordered {
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}
