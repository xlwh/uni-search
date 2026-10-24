// Package operator — Reconciler manages the UniSearchCluster CRD
// lifecycle (design 05 §8). A single Reconciler creates and manages
// StatefulSets, Services, and ConfigMaps for meta/coordinator/shard
// roles.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.5
package operator

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// UniSearchClusterReconciler reconciles a UniSearchCluster object.
type UniSearchClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is the main reconciliation loop. It ensures all child
// resources (StatefulSets, Services, ConfigMaps) exist and are in
// the desired state.
func (r *UniSearchClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &UniSearchCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure StatefulSets for each role.
	roles := []struct {
		name     string
		replicas int32
	}{
		{"meta", cluster.Spec.MetaReplicas},
		{"coordinator", cluster.Spec.CoordinatorReplicas},
		{"shard", cluster.Spec.ShardReplicas},
	}

	allReady := true
	for _, role := range roles {
		if err := r.ensureStatefulSet(ctx, cluster, role.name, role.replicas); err != nil {
			cluster.Status.Phase = "failed"
			cluster.Status.Ready = false
			_ = r.Status().Update(ctx, cluster)
			return ctrl.Result{}, err
		}
	}

	// Ensure headless Service for Raft discovery.
	if err := r.ensureService(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure ConfigMap for cluster topology.
	if err := r.ensureConfigMap(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	// Update status.
	if allReady {
		cluster.Status.Phase = "running"
		cluster.Status.Ready = true
		cluster.Status.LeaderAddr = fmt.Sprintf("%s-meta-0.%s.%s.svc.cluster.local",
			cluster.Name, cluster.Name, cluster.Namespace)
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ensureStatefulSet creates or updates a StatefulSet for the given role.
func (r *UniSearchClusterReconciler) ensureStatefulSet(ctx context.Context, cluster *UniSearchCluster, role string, replicas int32) error {
	if replicas <= 0 {
		replicas = 1
	}

	stsName := fmt.Sprintf("%s-%s", cluster.Name, role)
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Name: stsName, Namespace: cluster.Namespace}, sts)
	if err == nil {
		// Already exists; update replica count if needed.
		if *sts.Spec.Replicas != replicas {
			sts.Spec.Replicas = &replicas
			return r.Update(ctx, sts)
		}
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	// Create new StatefulSet.
	labels := labelsFor(cluster.Name, role)
	sts = &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stsName,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: fmt.Sprintf("%s-headless", cluster.Name),
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "unisearch",
						Image: cluster.Spec.Image,
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 9200},
							{Name: "grpc", ContainerPort: 9300},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						Env: []corev1.EnvVar{
							{Name: "UNISearch_ROLE", Value: role},
							{Name: "UNISEARCH_GRPC_ADDR", Value: ":9300"},
						},
					}},
				},
			},
		},
	}

	// Set controller reference for garbage collection.
	if err := controllerutil.SetControllerReference(cluster, sts, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, sts)
}

// ensureService creates a headless Service for Raft peer discovery.
func (r *UniSearchClusterReconciler) ensureService(ctx context.Context, cluster *UniSearchCluster) error {
	svcName := fmt.Sprintf("%s-headless", cluster.Name)
	svc := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKey{Name: svcName, Namespace: cluster.Namespace}, svc)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	labels := labelsFor(cluster.Name, "")
	svc = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None", // headless
			Selector:  labels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 9200, TargetPort: intstr.FromInt(9200)},
				{Name: "grpc", Port: 9300, TargetPort: intstr.FromInt(9300)},
			},
		},
	}

	if err := controllerutil.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, svc)
}

// ensureConfigMap creates a ConfigMap with cluster topology config.
func (r *UniSearchClusterReconciler) ensureConfigMap(ctx context.Context, cluster *UniSearchCluster) error {
	cmName := fmt.Sprintf("%s-config", cluster.Name)
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Name: cmName, Namespace: cluster.Namespace}, cm)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	labels := labelsFor(cluster.Name, "")
	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"cluster.yaml": fmt.Sprintf(`
metaReplicas: %d
coordinatorReplicas: %d
shardReplicas: %d
image: %s
objectStoreEndpoint: %s
bucket: %s
`,
				cluster.Spec.MetaReplicas,
				cluster.Spec.CoordinatorReplicas,
				cluster.Spec.ShardReplicas,
				cluster.Spec.Image,
				cluster.Spec.Storage.ObjectStoreEndpoint,
				cluster.Spec.Storage.Bucket),
		},
	}

	if err := controllerutil.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, cm)
}

// SetupWithManager registers the controller with the manager.
func (r *UniSearchClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&UniSearchCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

// labelsFor returns labels for a UniSearch cluster resource.
func labelsFor(clusterName, role string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":     "unisearch",
		"app.kubernetes.io/instance": clusterName,
	}
	if role != "" {
		labels["unisearch.db/role"] = role
	}
	return labels
}
