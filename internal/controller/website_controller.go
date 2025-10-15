/*
Copyright 2025 Matej Curcic.

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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstr "k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	webv1alpha1 "github.com/matej459/website-operator/api/v1alpha1"
)

const (
	componentName  = "website"
	cmKey          = "index.html"
	appLabelKey    = "app.kubernetes.io/name"
	instLabelKey   = "app.kubernetes.io/instance"
	partOfLabelKey = "app.kubernetes.io/part-of"

	annKey = "website.matej.nl/config-checksum"
)

// WebsiteReconciler reconciles a Website object
type WebsiteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=app.matej.nl,resources=websites,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=app.matej.nl,resources=websites/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=app.matej.nl,resources=websites/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *WebsiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.FromContext(ctx)

	var site webv1alpha1.Website
	var cfgHash string
	if err := r.Get(ctx, req.NamespacedName, &site); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
	}

	labels := map[string]string{
		appLabelKey:    componentName,
		instLabelKey:   site.Name,
		partOfLabelKey: "nginx-website",
	}

	// 1) ConfigMap (index.html)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      cmName(&site),
		Namespace: site.Namespace,
	}}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cmErr := controllerutil.SetControllerReference(&site, cm, r.Scheme); cmErr != nil {
			return cmErr
		}
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		for k, v := range labels {
			cm.Labels[k] = v
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		rendered := renderHTML(site.Spec.Message)
		cm.Data[cmKey] = rendered
		cfgHash = hashString(rendered)

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2) Deployment
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      deployName(&site),
		Namespace: site.Namespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		if depErr := controllerutil.SetControllerReference(&site, dep, r.Scheme); err != nil {
			return depErr
		}
		if dep.Labels == nil {
			dep.Labels = map[string]string{}
		}
		for k, v := range labels {
			dep.Labels[k] = v
		}
		selector := &metav1.LabelSelector{MatchLabels: map[string]string{
			appLabelKey:  componentName,
			instLabelKey: site.Name,
		}}

		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}

		dep.Spec.Template.Annotations[annKey] = cfgHash

		dep.Spec.Selector = selector
		dep.Spec.Replicas = site.Spec.Replicas
		dep.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}

		if site.Spec.Replicas != nil {
			dep.Spec.Replicas = site.Spec.Replicas
		}
		dep.Spec.Template.ObjectMeta.Labels = map[string]string{
			appLabelKey:  componentName,
			instLabelKey: site.Name,
		}
		dep.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  "nginx",
			Image: "nginx:stable",
			Ports: []corev1.ContainerPort{{ContainerPort: 80}},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "site",
				MountPath: "/usr/share/nginx/html/index.html",
				SubPath:   "index.html",
				ReadOnly:  true,
			}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(80)},
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(80)},
				},
			},
		}}
		dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
			Name: "site",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name},
					Items: []corev1.KeyToPath{{
						Key:  cmKey,
						Path: "index.html",
					}},
				},
			},
		}}
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3) Service
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      svcName(&site),
		Namespace: site.Namespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {

		if svcErr := controllerutil.SetControllerReference(&site, svc, r.Scheme); err != nil {
			return svcErr
		}
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		for k, v := range labels {
			svc.Labels[k] = v
		}
		svc.Spec.Selector = map[string]string{
			appLabelKey:  componentName,
			instLabelKey: site.Name,
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromInt(80),
		}}
		// ensure ClusterIP type (simple)
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	// 4) Status update
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local", svc.Name, svc.Namespace)

	if site.Status.URL != url || site.Status.ReadyReplicas != dep.Status.ReadyReplicas {
		site.Status.URL = url
		site.Status.ReadyReplicas = dep.Status.ReadyReplicas
		if err := r.Status().Update(ctx, &site); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WebsiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&webv1alpha1.Website{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func renderHTML(message string) string {
	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>Website</title></head>
<body><h1>%s</h1></body></html>`, message)
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func cmName(site *webv1alpha1.Website) string     { return site.Name + "-site" }
func deployName(site *webv1alpha1.Website) string { return site.Name + "-nginx" }
func svcName(site *webv1alpha1.Website) string    { return site.Name + "-svc" }
