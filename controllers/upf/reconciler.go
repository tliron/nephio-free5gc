/*
Copyright 2023 The Nephio Authors.

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

package upf

import (
	"context"
	"fmt"
	"time"

	nephiov1alpha1 "github.com/nephio-project/api/nf_deployments/v1alpha1"
	"github.com/nephio-project/free5gc/controllers"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Reconciles a UPFDeployment resource
type UPFDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Sets up the controller with the Manager
func (r *UPFDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(new(nephiov1alpha1.UPFDeployment)).
		Owns(new(appsv1.Deployment)).
		Owns(new(apiv1.ConfigMap)).
		Complete(r)
}

//+kubebuilder:rbac:groups=workload.nephio.org,resources=upfdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=workload.nephio.org,resources=upfdeployments/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="k8s.cni.cncf.io",resources=network-attachment-definitions,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the UPFDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *UPFDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("UPFDeployment", req.NamespacedName)

	upfDeployment := new(nephiov1alpha1.UPFDeployment)
	err := r.Client.Get(ctx, req.NamespacedName, upfDeployment)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			log.Info("UPFDeployment resource not found. Ignoring since object must be deleted")
			return reconcile.Result{}, nil
		}
		log.Error(err, "Failed to get UPFDeployment")
		return reconcile.Result{}, err
	}

	namespace := upfDeployment.Namespace

	configMapFound := false
	configMapName := upfDeployment.Name + "-upf-configmap"
	var configMapVersion string
	currentConfigMap := new(apiv1.ConfigMap)
	if err := r.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, currentConfigMap); err == nil {
		configMapFound = true
		configMapVersion = currentConfigMap.ResourceVersion
	}

	deploymentFound := false
	deploymentName := upfDeployment.Name
	currentDeployment := new(appsv1.Deployment)
	if err := r.Client.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, currentDeployment); err == nil {
		deploymentFound = true
	}

	if deploymentFound {
		deployment := currentDeployment.DeepCopy()

		// Updating UPFDeployment status. On the first sets the first Condition to Reconciling.
		// On the subsequent runs it gets undelying depoyment Conditions and use the last one to decide if status has to be updated.
		if deployment.DeletionTimestamp == nil {
			if err := r.syncStatus(ctx, deployment, upfDeployment); err != nil {
				log.Error(err, "Failed to update UPFDeployment status", "UPFDeployment.namespace", namespace, "UPFDeployment.name", upfDeployment.Name)
				return reconcile.Result{}, err
			}
		}

		if currentDeployment.Spec.Template.Annotations[controllers.ConfigMapVersionAnnotation] != configMapVersion {
			log.Info("ConfigMap has been updated. Rolling Deployment pods.", "UPFDeployment.namespace", namespace, "UPFDeployment.name", upfDeployment.Name)
			currentDeployment.Spec.Template.Annotations[controllers.ConfigMapVersionAnnotation] = configMapVersion

			if err := r.Update(ctx, currentDeployment); err != nil {
				log.Error(err, "Failed to update Deployment", "UPFDeployment.namespace", currentDeployment.Namespace, "UPFDeployment.name", currentDeployment.Name)
				return reconcile.Result{}, err
			}

			return reconcile.Result{Requeue: true}, nil
		}
	}

	if configMap, err := createConfigMap(log, upfDeployment); err == nil {
		if !configMapFound {
			log.Info("Creating UPFDeployment configmap", "UPFDeployment.namespace", namespace, "ConfigMap.name", configMap.Name)

			// Set the controller reference, specifying that UPFDeployment controling underlying ConfigMap
			if err := ctrl.SetControllerReference(upfDeployment, configMap, r.Scheme); err != nil {
				log.Error(err, "Got error while setting Owner reference on ConfigMap.", "UPFDeployment.namespace", namespace)
			}

			if err := r.Client.Create(ctx, configMap); err != nil {
				log.Error(err, fmt.Sprintf("Failed to create ConfigMap %s\n", err.Error()))
				return reconcile.Result{}, err
			}

			configMapVersion = configMap.ResourceVersion
		}
	} else {
		log.Error(err, fmt.Sprintf("Failed to create ConfigMap %s\n", err.Error()))
		return reconcile.Result{}, err
	}

	if deployment, err := createDeployment(log, configMapVersion, upfDeployment); err == nil {
		if !deploymentFound {
			// Only create Deployment in case all required NADs are present. Otherwise Requeue in 10 sec.
			if ok := controllers.ValidateNetworkAttachmentDefinitions(ctx, r.Client, log, upfDeployment.Kind, deployment); ok {
				// Set the controller reference, specifying that UPFDeployment controls the underlying Deployment
				if err := ctrl.SetControllerReference(upfDeployment, deployment, r.Scheme); err != nil {
					log.Error(err, "Got error while setting Owner reference on Deployment.", "UPFDeployment.namespace", namespace)
				}

				log.Info("Creating UPFDeployment", "UPFDeployment.namespace", namespace, "UPFDeployment.name", upfDeployment.Name)
				if err := r.Client.Create(ctx, deployment); err != nil {
					log.Error(err, "Failed to create new Deployment", "UPFDeployment.namespace", namespace, "UPFDeployment.name", upfDeployment.Name)
				}

				// TODO(tliron): explain why we need requeueing (do we?)
				return reconcile.Result{RequeueAfter: time.Duration(30) * time.Second}, nil
			} else {
				log.Info("Not all NetworkAttachDefinitions available in current namespace. Requeue in 10 sec.", "UPFDeployment.namespace", namespace)
				return reconcile.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
			}
		}
	} else {
		log.Error(err, fmt.Sprintf("Failed to create Deployment %s\n", err.Error()))
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *UPFDeploymentReconciler) syncStatus(ctx context.Context, deployment *appsv1.Deployment, upfDeployment *nephiov1alpha1.UPFDeployment) error {
	if nfDeploymentStatus, update := createNfDeploymentStatus(deployment, upfDeployment); update {
		upfDeployment = upfDeployment.DeepCopy()
		upfDeployment.Status.NFDeploymentStatus = nfDeploymentStatus
		return r.Status().Update(ctx, upfDeployment)
	} else {
		return nil
	}
}