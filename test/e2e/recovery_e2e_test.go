//go:build e2e

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

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jihyun-huh/gateway-api-openstack/internal/audit"
)

var errEvidenceNotConfigured = errors.New("evidence input was not configured")

const maximumAuditOutputBytes = 16 << 20

type boundedAuditBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedAuditBuffer) Write(contents []byte) (int, error) {
	available := b.limit + 1 - b.buffer.Len()
	if available > 0 {
		written := len(contents)
		if written > available {
			written = available
		}
		_, _ = b.buffer.Write(contents[:written])
	}
	if b.buffer.Len() > b.limit || len(contents) > available {
		b.overflow = true
	}
	return len(contents), nil
}

func (b *boundedAuditBuffer) Bytes() []byte { return b.buffer.Bytes() }

type leaderProcessState struct {
	podUID       types.UID
	startedAt    time.Time
	restartCount int32
}

func (s *phase2Suite) deleteLeaderAndVerifyRecovery(ctx context.Context) error {
	if err := s.verifyControllerDeployment(ctx); err != nil {
		return err
	}
	deployment, pods, err := s.readyControllerPods(ctx)
	if err != nil {
		return err
	}
	if err := s.validateControllerDeploymentIdentity(deployment); err != nil {
		return err
	}
	before, err := s.currentLease(ctx)
	if err != nil {
		return err
	}
	leader, err := matchLeaseHolder(before.holder, pods)
	if err != nil {
		return err
	}
	oldLeaderName := leader.Name
	leaderUID := leader.UID
	if err := s.client.Delete(ctx, leader, client.Preconditions{UID: &leaderUID}); err != nil {
		return fmt.Errorf("delete current controller leader pod: %w", err)
	}

	err = wait.PollUntilContextCancel(ctx, s.config.pollInterval, true, func(ctx context.Context) (bool, error) {
		var old corev1.Pod
		err := s.client.Get(ctx, client.ObjectKey{Namespace: s.config.controllerNamespace, Name: oldLeaderName}, &old)
		if err == nil || !apierrors.IsNotFound(err) {
			return false, nil
		}
		if err := s.verifyControllerDeployment(ctx); err != nil {
			return false, nil
		}
		_, currentPods, err := s.readyControllerPods(ctx)
		if err != nil {
			return false, nil
		}
		after, err := s.currentLease(ctx)
		if err != nil || after.holder == before.holder || after.transitions <= before.transitions {
			return false, nil
		}
		newLeader, err := matchLeaseHolder(after.holder, currentPods)
		return err == nil && newLeader.Name != oldLeaderName, nil
	})
	if err != nil {
		return fmt.Errorf("wait for leader replacement: %w", err)
	}
	if err := s.verifyWorkloadAfterRecovery(ctx); err != nil {
		return err
	}
	return s.verifyRecoveryAudit(ctx, "leader")
}

func (s *phase2Suite) coldRestartAndVerifyRecovery(ctx context.Context) (retErr error) {
	s.controllerScaled = true
	defer func() {
		if !s.controllerScaled {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.restoreAndVerifyController(restoreCtx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore controller after cold restart failure: %w", err))
		}
	}()
	if err := s.scaleController(ctx, 0); err != nil {
		return err
	}
	if err := wait.PollUntilContextCancel(ctx, s.config.pollInterval, true, func(ctx context.Context) (bool, error) {
		stopped, err := s.controllerDeploymentStopped(ctx)
		if err != nil {
			return false, nil
		}
		return stopped, nil
	}); err != nil {
		return fmt.Errorf("wait for cold controller stop: %w", err)
	}
	if err := s.verifyExternalTraffic(ctx); err != nil {
		return fmt.Errorf("verify data plane while controller is stopped: %w", err)
	}
	if err := s.restoreAndVerifyController(ctx); err != nil {
		return err
	}
	if err := s.verifyWorkloadAfterRecovery(ctx); err != nil {
		return err
	}
	return s.verifyRecoveryAudit(ctx, "restart")
}

func (s *phase2Suite) controllerDeploymentStopped(ctx context.Context) (bool, error) {
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: s.config.controllerNamespace, Name: s.config.controllerDeployment}
	if err := s.client.Get(ctx, key, &deployment); err != nil {
		return false, err
	}
	if err := s.validateControllerDeploymentIdentity(&deployment); err != nil {
		return false, err
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 ||
		deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.Replicas != 0 || deployment.Status.ReadyReplicas != 0 ||
		deployment.Status.AvailableReplicas != 0 {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return false, fmt.Errorf("build controller Deployment selector: %w", err)
	}
	var pods corev1.PodList
	if err := s.client.List(ctx, &pods,
		client.InNamespace(s.config.controllerNamespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

func (s *phase2Suite) verifyWorkloadAfterRecovery(ctx context.Context) error {
	if err := s.waitForGatewayClass(ctx); err != nil {
		return err
	}
	if err := s.waitForGateway(ctx, 1); err != nil {
		return err
	}
	if err := s.waitForHTTPRoute(ctx); err != nil {
		return err
	}
	return s.verifyExternalTraffic(ctx)
}

func (s *phase2Suite) scaleController(ctx context.Context, replicas int32) error {
	key := client.ObjectKey{Namespace: s.config.controllerNamespace, Name: s.config.controllerDeployment}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var deployment appsv1.Deployment
		if err := s.client.Get(ctx, key, &deployment); err != nil {
			return err
		}
		if err := s.validateControllerDeploymentIdentity(&deployment); err != nil {
			return err
		}
		deployment.Spec.Replicas = &replicas
		return s.client.Update(ctx, &deployment)
	}); err != nil {
		return fmt.Errorf("scale controller Deployment: %w", err)
	}
	return nil
}

func (s *phase2Suite) restoreAndVerifyController(ctx context.Context) error {
	if err := s.scaleController(ctx, s.config.controllerReplicas); err != nil {
		return err
	}
	if err := s.waitForControllerHealthy(ctx); err != nil {
		return err
	}
	s.controllerScaled = false
	return nil
}

func (s *phase2Suite) waitForControllerHealthy(ctx context.Context) error {
	if err := wait.PollUntilContextCancel(ctx, s.config.pollInterval, true, func(ctx context.Context) (bool, error) {
		if err := s.verifyControllerDeployment(ctx); err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("wait for controller restoration and leader election: %w", err)
	}
	return nil
}

func (s *phase2Suite) currentLease(ctx context.Context) (leaseState, error) {
	var lease coordinationv1.Lease
	key := client.ObjectKey{Namespace: s.config.controllerNamespace, Name: s.config.leaderLease}
	if err := s.client.Get(ctx, key, &lease); err != nil {
		return leaseState{}, fmt.Errorf("read controller leader Lease: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || strings.TrimSpace(*lease.Spec.HolderIdentity) == "" ||
		lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return leaseState{}, fmt.Errorf("controller leader Lease is incomplete")
	}
	expiresAt := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	if !time.Now().Before(expiresAt) {
		return leaseState{}, fmt.Errorf("controller leader Lease is expired")
	}
	state := leaseState{holder: *lease.Spec.HolderIdentity}
	if lease.Spec.LeaseTransitions != nil {
		state.transitions = *lease.Spec.LeaseTransitions
	}
	return state, nil
}

func matchLeaseHolder(holder string, pods []corev1.Pod) (*corev1.Pod, error) {
	var match *corev1.Pod
	for index := range pods {
		if holder != pods[index].Name && !strings.HasPrefix(holder, pods[index].Name+"_") {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("leader Lease holder matches more than one controller pod")
		}
		match = &pods[index]
	}
	if match == nil || !podReady(match) {
		return nil, fmt.Errorf("leader Lease holder is not a Ready pod from the configured Deployment")
	}
	return match, nil
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func findContainer(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, container := range containers {
		if container.Name == name {
			return container, true
		}
	}
	return corev1.Container{}, false
}

func findContainerStatus(statuses []corev1.ContainerStatus, name string) (corev1.ContainerStatus, bool) {
	for _, status := range statuses {
		if status.Name == name {
			return status, true
		}
	}
	return corev1.ContainerStatus{}, false
}

func (s *phase2Suite) verifyConvergedNoOp(ctx context.Context) error {
	before, beforeLease, beforeProcess, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	if err := s.triggerConvergedRouteObservation(ctx); err != nil {
		return err
	}
	timer := time.NewTimer(s.config.noOpWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	after, afterLease, afterProcess, err := s.scrapeCurrentLeaderMetrics(ctx)
	if err != nil {
		return err
	}
	if beforeLease.holder != afterLease.holder || beforeLease.transitions != afterLease.transitions {
		return fmt.Errorf("leader Lease changed during the converged observation window")
	}
	if beforeProcess != afterProcess {
		return fmt.Errorf("leader Pod or controller container changed during the converged observation window")
	}
	if err := validateConvergedNoOp(before, after, s.config.leaderLease); err != nil {
		return err
	}
	s.report.Metrics = &metricsEvidence{
		Before: cloneMetricSnapshot(before.values),
		After:  cloneMetricSnapshot(after.values),
	}
	return nil
}

func (s *phase2Suite) scrapeCurrentLeaderMetrics(ctx context.Context) (metricSnapshot, leaseState, leaderProcessState, error) {
	if err := s.verifyControllerDeployment(ctx); err != nil {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, err
	}
	_, pods, err := s.readyControllerPods(ctx)
	if err != nil {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, err
	}
	lease, err := s.currentLease(ctx)
	if err != nil {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, err
	}
	leader, err := matchLeaseHolder(lease.holder, pods)
	if err != nil {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, err
	}
	status, found := findContainerStatus(leader.Status.ContainerStatuses, s.config.controllerContainer)
	if !found || status.State.Running == nil || status.State.Running.StartedAt.IsZero() {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, fmt.Errorf("leader controller container has no stable running state")
	}
	process := leaderProcessState{
		podUID:       leader.UID,
		startedAt:    status.State.Running.StartedAt.Time,
		restartCount: status.RestartCount,
	}
	stream, err := s.coreRESTClient.Get().
		Namespace(s.config.controllerNamespace).
		Resource("pods").
		Name(fmt.Sprintf("%s:%d", leader.Name, controllerMetricsPort)).
		SubResource("proxy").
		Suffix("metrics").
		Stream(ctx)
	if err != nil {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, fmt.Errorf("open current leader metrics proxy: %w", err)
	}
	defer func() { _ = stream.Close() }()
	snapshot, err := readMetricSnapshot(stream)
	if err != nil {
		return metricSnapshot{}, leaseState{}, leaderProcessState{}, err
	}
	return snapshot, lease, process, nil
}

func (s *phase2Suite) triggerConvergedRouteObservation(ctx context.Context) error {
	key := client.ObjectKey{Namespace: s.config.namespace, Name: routeName}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var route gatewayv1.HTTPRoute
		if err := s.client.Get(ctx, key, &route); err != nil {
			return err
		}
		if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].BackendRefs) != 1 {
			return fmt.Errorf("HTTPRoute no longer has the expected baseline shape")
		}
		if route.Spec.Rules[0].BackendRefs[0].Namespace != nil {
			return fmt.Errorf("HTTPRoute backendRef already has an explicit namespace")
		}
		namespace := gatewayv1.Namespace(s.config.namespace)
		route.Spec.Rules[0].BackendRefs[0].Namespace = &namespace
		return s.client.Update(ctx, &route)
	}); err != nil {
		return fmt.Errorf("trigger semantically converged HTTPRoute observation: %w", err)
	}
	if err := s.waitForHTTPRoute(ctx); err != nil {
		return fmt.Errorf("wait for converged HTTPRoute observation: %w", err)
	}
	return s.waitForGateway(ctx, 1)
}

func cloneMetricSnapshot(input map[string]float64) map[string]float64 {
	output := make(map[string]float64, len(input))
	for name, value := range input {
		output[name] = value
	}
	return output
}

func (s *phase2Suite) verifyActiveOwnershipInventoryAudit(ctx context.Context) error {
	if !s.config.audit.enabled {
		return errEvidenceNotConfigured
	}
	var fingerprint [32]byte
	summary, err := s.runOwnershipAudit(ctx, false, &fingerprint)
	if err != nil {
		return err
	}
	if summary.Bindings != 2 || summary.Resources == 0 || summary.Matched != summary.Resources {
		return fmt.Errorf("ownership audit did not observe the expected stable active ownership inventory")
	}
	s.activeAudit = &summary
	s.activeAuditFingerprint = fingerprint
	if s.report.Audit == nil {
		s.report.Audit = &auditEvidence{}
	}
	s.report.Audit.Active = &summary
	return nil
}

func (s *phase2Suite) verifyRecoveryAudit(ctx context.Context, stage string) error {
	if s.activeAudit == nil {
		return nil
	}
	var observed auditSummary
	if err := wait.PollUntilContextCancel(ctx, s.config.pollInterval, true, func(ctx context.Context) (bool, error) {
		var fingerprint [32]byte
		summary, err := s.runOwnershipAudit(ctx, false, &fingerprint)
		if err != nil {
			return false, nil
		}
		observed = summary
		return reflect.DeepEqual(summary, *s.activeAudit) && fingerprint == s.activeAuditFingerprint, nil
	}); err != nil {
		return fmt.Errorf("wait for active ownership inventory after controller recovery: %w", err)
	}
	switch stage {
	case "leader":
		s.report.Audit.AfterLeader = &observed
	case "restart":
		s.report.Audit.AfterRestart = &observed
	default:
		return fmt.Errorf("unsupported ownership audit recovery stage")
	}
	return nil
}

func (s *phase2Suite) orderlyCleanup(ctx context.Context) error {
	if err := s.waitForControllerHealthy(ctx); err != nil {
		return fmt.Errorf("verify controller before finalizer-bound cleanup: %w", err)
	}
	if err := s.deleteHTTPRoute(ctx); err != nil {
		return err
	}
	if err := s.deleteGateway(ctx); err != nil {
		return err
	}
	if err := s.deleteGatewayClass(ctx); err != nil {
		return err
	}
	return s.deleteNamespace(ctx)
}

func (s *phase2Suite) ensureCleanup(ctx context.Context) error {
	if s.controllerScaled {
		if err := s.restoreAndVerifyController(ctx); err != nil {
			return fmt.Errorf("restore controller before finalizer-bound cleanup: %w", err)
		}
	} else if s.createdRoute || s.createdGateway {
		if err := s.waitForControllerHealthy(ctx); err != nil {
			return fmt.Errorf("verify controller before finalizer-bound cleanup: %w", err)
		}
	}
	for _, cleanup := range []func(context.Context) error{
		s.deleteHTTPRoute,
		s.deleteGateway,
		s.deleteGatewayClass,
		s.deleteNamespace,
	} {
		if err := cleanup(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *phase2Suite) deleteHTTPRoute(ctx context.Context) error {
	if !s.createdRoute {
		return nil
	}
	object := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: s.config.namespace, Name: routeName}}
	if err := s.deleteMarkedAndWait(ctx, object, &s.routeUID); err != nil {
		return fmt.Errorf("delete HTTPRoute and wait for finalizers: %w", err)
	}
	s.createdRoute = false
	return nil
}

func (s *phase2Suite) deleteGateway(ctx context.Context) error {
	if !s.createdGateway {
		return nil
	}
	object := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: s.config.namespace, Name: gatewayName}}
	if err := s.deleteMarkedAndWait(ctx, object, &s.gatewayUID); err != nil {
		return fmt.Errorf("delete Gateway and wait for finalizers: %w", err)
	}
	s.createdGateway = false
	return nil
}

func (s *phase2Suite) deleteGatewayClass(ctx context.Context) error {
	if !s.createdClass {
		return nil
	}
	object := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: s.gatewayClassName}}
	if err := s.deleteMarkedAndWait(ctx, object, &s.gatewayClassUID); err != nil {
		return fmt.Errorf("delete GatewayClass: %w", err)
	}
	s.createdClass = false
	return nil
}

func (s *phase2Suite) deleteNamespace(ctx context.Context) error {
	if !s.createdNamespace {
		return nil
	}
	object := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.config.namespace}}
	if err := s.deleteMarkedAndWait(ctx, object, &s.namespaceUID); err != nil {
		return fmt.Errorf("delete isolated Namespace: %w", err)
	}
	s.createdNamespace = false
	return nil
}

func (s *phase2Suite) deleteMarkedAndWait(ctx context.Context, object client.Object, expectedUID *types.UID) error {
	key := client.ObjectKeyFromObject(object)
	fresh := object.DeepCopyObject().(client.Object)
	if err := s.client.Get(ctx, key, fresh); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if fresh.GetAnnotations()[dedicatedRunAnnotation] != s.config.runID {
		return fmt.Errorf("refuse cleanup because the object lacks the exact run annotation")
	}
	uid := fresh.GetUID()
	if uid == "" {
		return fmt.Errorf("refuse cleanup because the object has no immutable UID")
	}
	if *expectedUID != "" && *expectedUID != uid {
		return fmt.Errorf("refuse cleanup because the object UID changed")
	}
	*expectedUID = uid
	if err := s.client.Delete(ctx, fresh, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return wait.PollUntilContextCancel(ctx, s.config.pollInterval, true, func(ctx context.Context) (bool, error) {
		copy := object.DeepCopyObject().(client.Object)
		err := s.client.Get(ctx, key, copy)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if copy.GetUID() != uid {
			return false, fmt.Errorf("same-name object was recreated during cleanup")
		}
		return false, nil
	})
}

func (s *phase2Suite) runOwnershipAudit(ctx context.Context, requireEmpty bool, fingerprint *[32]byte) (auditSummary, error) {
	if err := s.verifyAuthenticatedSharedProject(ctx); err != nil {
		return auditSummary{}, err
	}
	arguments := []string{
		"--controller-name=" + s.config.controllerName,
		"--cluster-id=" + s.config.audit.clusterID,
		"--kubeconfig=" + s.config.kubeconfig,
		"--context=" + s.config.kubeContext,
		"--octavia-microversion=" + s.config.audit.microversion,
	}
	if s.config.audit.cloudsYAML != "" {
		arguments = append(arguments, "--clouds-yaml="+s.config.audit.cloudsYAML)
	}
	if s.config.audit.cloud != "" {
		arguments = append(arguments, "--openstack-cloud="+s.config.audit.cloud)
	}
	if s.config.audit.region != "" {
		arguments = append(arguments, "--openstack-region="+s.config.audit.region)
	}
	command := exec.CommandContext(ctx, s.config.audit.binary, arguments...)
	stdout := boundedAuditBuffer{limit: maximumAuditOutputBytes}
	stderr := boundedAuditBuffer{limit: maximumAuditOutputBytes}
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return auditSummary{}, fmt.Errorf("ownership audit command failed")
	}
	if stdout.overflow || stderr.overflow {
		return auditSummary{}, fmt.Errorf("ownership audit output exceeded the safe in-memory limit")
	}
	var report audit.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return auditSummary{}, fmt.Errorf("ownership audit output was not valid JSON")
	}
	if report.FormatVersion != audit.ReportFormatVersion || report.Assessment != audit.AssessmentComplete {
		return auditSummary{}, fmt.Errorf("ownership audit did not produce a complete supported report")
	}
	if report.Scope.ClusterID != s.config.audit.clusterID || report.Scope.ControllerName != s.config.controllerName {
		return auditSummary{}, fmt.Errorf("ownership audit report scope did not match the configured run")
	}
	if report.HasFindings() {
		return auditSummary{}, fmt.Errorf("ownership audit reported unresolved findings")
	}
	if requireEmpty && report.Summary != (audit.Summary{}) {
		return auditSummary{}, fmt.Errorf("run-scoped ownership audit baseline was not empty")
	}
	if fingerprint != nil {
		*fingerprint = ownershipAuditFingerprint(report.Resources)
	}
	return sanitizeAuditSummary(report), nil
}

func ownershipAuditFingerprint(resources []audit.ResourceFinding) [32]byte {
	keys := make([]string, 0, len(resources))
	for _, resource := range resources {
		objectKeys := make([]string, 0, len(resource.Objects))
		for _, object := range resource.Objects {
			objectKeys = append(objectKeys, strings.Join([]string{
				object.APIVersion,
				object.Kind,
				object.Namespace,
				object.Name,
				object.UID,
			}, "\x00"))
		}
		sort.Strings(objectKeys)
		keys = append(keys, strings.Join([]string{
			resource.Service,
			resource.Type,
			resource.ID,
			resource.ParentID,
			resource.Role,
			resource.ProvisioningStatus,
			string(resource.Disposition),
			strings.Join(objectKeys, "\x01"),
		}, "\x00"))
	}
	sort.Strings(keys)
	return sha256.Sum256([]byte(strings.Join(keys, "\x02")))
}

func sanitizeAuditSummary(report audit.Report) auditSummary {
	return auditSummary{
		Assessment:       string(report.Assessment),
		Bindings:         report.Summary.KubernetesBindings,
		KubernetesIssues: report.Summary.KubernetesIssues,
		Resources:        report.Summary.OpenStackResources,
		Matched:          report.Summary.Matched,
		OrphanCandidates: report.Summary.OrphanCandidates,
		StaleUIDs:        report.Summary.StaleUIDs,
		Conflicts:        report.Summary.OwnershipConflicts,
		Unresolved:       report.Summary.Unresolved,
	}
}

func (s *phase2Suite) verifyPostCleanupAudit(ctx context.Context) error {
	var observed auditSummary
	err := wait.PollUntilContextCancel(ctx, s.config.pollInterval, true, func(ctx context.Context) (bool, error) {
		summary, err := s.runOwnershipAudit(ctx, true, nil)
		if err != nil {
			return false, nil
		}
		observed = summary
		return reflect.DeepEqual(summary, *s.baselineAudit), nil
	})
	if err != nil {
		return fmt.Errorf("wait for ownership audit to return to baseline: %w", err)
	}
	s.report.Audit.AfterCleanup = &observed
	mustSetCheck(s.report, "post-test ownership audit returns to baseline", statusPassed, passedSummary("post-test ownership audit returns to baseline"))
	return nil
}

func TestBoundedAuditBufferStopsRetainingOutputAtLimit(t *testing.T) {
	buffer := boundedAuditBuffer{limit: 4}
	if written, err := buffer.Write([]byte("123456789")); err != nil || written != 9 {
		t.Fatalf("boundedAuditBuffer.Write() = %d, %v", written, err)
	}
	if !buffer.overflow || len(buffer.Bytes()) != 5 {
		t.Fatalf("bounded buffer overflow = %t, retained = %d, want true and 5", buffer.overflow, len(buffer.Bytes()))
	}
}

func TestOwnershipAuditFingerprintIsDeterministicAndIdentitySensitive(t *testing.T) {
	first := audit.ResourceFinding{Service: "octavia", Type: "listener", ID: "resource-a", ParentID: "parent-a", Role: "listener", Disposition: audit.DispositionMatched}
	second := audit.ResourceFinding{Service: "octavia", Type: "pool", ID: "resource-b", ParentID: "resource-a", Role: "pool", Disposition: audit.DispositionMatched}
	want := ownershipAuditFingerprint([]audit.ResourceFinding{first, second})
	if got := ownershipAuditFingerprint([]audit.ResourceFinding{second, first}); got != want {
		t.Fatal("ownershipAuditFingerprint() depended on inventory order")
	}
	second.ID = "resource-c"
	if got := ownershipAuditFingerprint([]audit.ResourceFinding{first, second}); got == want {
		t.Fatal("ownershipAuditFingerprint() ignored a resource identity change")
	}
}

func TestMatchLeaseHolderRequiresOneReadyPod(t *testing.T) {
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-a"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}}
	matched, err := matchLeaseHolder("controller-a_unique", pods)
	if err != nil || matched.Name != "controller-a" {
		t.Fatalf("matchLeaseHolder() = %#v, %v", matched, err)
	}
	pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := matchLeaseHolder("controller-a_unique", pods); err == nil {
		t.Fatal("matchLeaseHolder() accepted a non-Ready Pod")
	}
}
