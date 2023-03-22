/*
Copyright 2023.

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

package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bigdatav1alpha1 "github.com/kubernetesbigdataeg/solr-operator/api/v1alpha1"
)

const solrFinalizer = "bigdata.kubernetesbigdataeg.org/finalizer"

// Definitions to manage status conditions
const (
	// typeAvailableSolr represents the status of the Deployment reconciliation
	typeAvailableSolr = "Available"
	// typeDegradedSolr represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedSolr = "Degraded"
)

// SolrReconciler reconciles a Solr object
type SolrReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=bigdata.kubernetesbigdataeg.org,resources=solrs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=bigdata.kubernetesbigdataeg.org,resources=solrs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=bigdata.kubernetesbigdataeg.org,resources=solrs/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps;services,verbs=get;list;create;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *SolrReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	//
	// 1. Control-loop: checking if Solr CR exists
	//
	// Fetch the Solr instance
	// The purpose is check if the Custom Resource for the Kind Solr
	// is applied on the cluster if not we return nil to stop the reconciliation
	solr := &bigdatav1alpha1.Solr{}
	err := r.Get(ctx, req.NamespacedName, solr)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("solr resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get solr")
		return ctrl.Result{}, err
	}

	//
	// 2. Control-loop: Status to Unknown
	//
	// Let's just set the status as Unknown when no status are available
	if solr.Status.Conditions == nil || len(solr.Status.Conditions) == 0 {
		meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, solr); err != nil {
			log.Error(err, "Failed to update Solr status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the solr Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, solr); err != nil {
			log.Error(err, "Failed to re-fetch solr")
			return ctrl.Result{}, err
		}
	}

	//
	// 3. Control-loop: Let's add a finalizer
	//
	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(solr, solrFinalizer) {
		log.Info("Adding Finalizer for Solr")
		if ok := controllerutil.AddFinalizer(solr, solrFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, solr); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	//
	// 4. Control-loop: Instance marked for deletion
	//
	// Check if the Solr instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isSolrMarkedToBeDeleted := solr.GetDeletionTimestamp() != nil
	if isSolrMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(solr, solrFinalizer) {
			log.Info("Performing Finalizer Operations for Solr before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeDegradedSolr,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", solr.Name)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForSolr(solr)

			// TODO(user): If you add operations to the doFinalizerOperationsForSolr method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the solr Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, solr); err != nil {
				log.Error(err, "Failed to re-fetch solr")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeDegradedSolr,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", solr.Name)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Solr after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(solr, solrFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Solr")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, solr); err != nil {
				log.Error(err, "Failed to remove finalizer for Solr")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	//
	// 5. Control-loop: Let's deploy/ensure our managed resources for Solr
	// - ConfigMap,
	// - Service Headless,
	// - Service ClusterIP,
	// - StateFulSet
	//

	//ConfigMap: Check if the ConfigMap already exists, if not create a new one
	configMapFound := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: "solr-cluster-config", Namespace: solr.Namespace}, configMapFound)
	if err != nil && apierrors.IsNotFound(err) {
		// Define the default ConfigMap
		cm, err := r.defaultConfigMapForSolr(solr)
		if err != nil {
			log.Error(err, "Failed to define new ConfigMap resource for Solr")

			// The following implementation will update the status
			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create ConfigMap for the custom resource (%s): (%s)",
					solr.Name, err)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new ConfigMap",
			"ConfigMap.Namespace", cm.Namespace, "ConfigMap.Name", cm.Name)

		if err = r.Create(ctx, cm); err != nil {
			log.Error(err, "Failed to create new ConfigMap",
				"ConfigMap.Namespace", cm.Namespace, "ConfigMap.Name", cm.Name)
			return ctrl.Result{}, err
		}

		// ConfigMap created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		//return ctrl.Result{RequeueAfter: time.Minute}, nil

	} else if err != nil {
		log.Error(err, "Failed to get ConfigMap")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// Service Headless: Check if the headless svc already exists, if not create a new one
	serviceHeadlessFound := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: "solrcluster", Namespace: solr.Namespace}, serviceHeadlessFound)
	if err != nil && apierrors.IsNotFound(err) {

		hsvc, err := r.serviceHeadlessForSolr(solr)
		if err != nil {
			log.Error(err, "Failed to define new Headless Service resource for Solr")

			// The following implementation will update the status
			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Headless Service for the custom resource (%s): (%s)",
					solr.Name, err)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Headless Service",
			"Service.Namespace", hsvc.Namespace, "Service.Name", hsvc.Name)

		if err = r.Create(ctx, hsvc); err != nil {
			log.Error(err, "Failed to create new Headless Service",
				"Service.Namespace", hsvc.Namespace, "Service.Name", hsvc.Name)
			return ctrl.Result{}, err
		}

		// Service created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		//return ctrl.Result{RequeueAfter: time.Minute}, nil

	} else if err != nil {
		log.Error(err, "Failed to get Headless Service")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// Service: Check if the headless svc already exists, if not create a new one
	serviceFound := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: "solr-svc", Namespace: solr.Namespace}, serviceFound)
	if err != nil && apierrors.IsNotFound(err) {

		svc, err := r.serviceForSolr(solr)
		if err != nil {
			log.Error(err, "Failed to define new Service resource for Solr")

			// The following implementation will update the status
			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Service for the custom resource (%s): (%s)",
					solr.Name, err)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Service",
			"Service.Namespace", svc.Namespace, "Service.Name", svc.Name)

		if err = r.Create(ctx, svc); err != nil {
			log.Error(err, "Failed to create new Service",
				"Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
			return ctrl.Result{}, err
		}

		// Service created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		//return ctrl.Result{RequeueAfter: time.Minute}, nil

	} else if err != nil {
		log.Error(err, "Failed to get Headless Service")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// StateFulSet: Check if the sts already exists, if not create a new one
	found := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: solr.Name, Namespace: solr.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new sts
		dep, err := r.stateFulSetForSolr(solr)
		if err != nil {
			log.Error(err, "Failed to define new StateFulSet resource for Solr")

			// The following implementation will update the status
			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create StatefulSet for the custom resource (%s): (%s)", solr.Name, err)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new StateFulSet",
			"StatefulSet.Namespace", dep.Namespace, "StatefulSet.Name", dep.Name)

		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new StatefulSet",
				"StatefulSet.Namespace", dep.Namespace, "StatefulSet.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// StatefulSet created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get StatefulSet")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// The CRD API is defining that the Solr type, have a SolrSpec.Size field
	// to set the quantity of Deployment instances is the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment size is the same as defined
	// via the Size spec of the Custom Resource which we are reconciling.
	size := solr.Spec.Size
	if *found.Spec.Replicas != size {
		found.Spec.Replicas = &size
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Failed to update Deployment",
				"Deployment.Namespace", found.Namespace, "Deployment.Name", found.Name)

			// Re-fetch the solr Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, solr); err != nil {
				log.Error(err, "Failed to re-fetch solr")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr,
				Status: metav1.ConditionFalse, Reason: "Resizing",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", solr.Name, err)})

			if err := r.Status().Update(ctx, solr); err != nil {
				log.Error(err, "Failed to update Solr status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	// The following implementation will update the status
	meta.SetStatusCondition(&solr.Status.Conditions, metav1.Condition{Type: typeAvailableSolr,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", solr.Name, size)})

	if err := r.Status().Update(ctx, solr); err != nil {
		log.Error(err, "Failed to update Solr status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// finalizeSolr will perform the required operations before delete the CR.
func (r *SolrReconciler) doFinalizerOperationsForSolr(cr *bigdatav1alpha1.Solr) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

func (r *SolrReconciler) defaultConfigMapForSolr(v *bigdatav1alpha1.Solr) (*corev1.ConfigMap, error) {

	//solr := &bigdatav1alpha1.Solr{}
	//zookeeper := solr.Spec.Zookeeper

	zookeeperConnect := ``
	for i := 0; i <= 2; i++ {
		literal :=
			//strings.Split(zookeeper, "/")[1] + `-` +
			`zk-` +
				strconv.Itoa(i) + `.zk-hs.default` +
				//strings.Split(zookeeper, "/")[0] +
				`.svc.cluster.local:2181`
		if zookeeperConnect == `` {
			zookeeperConnect = literal
		} else {
			zookeeperConnect = zookeeperConnect + "," + literal
		}

	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "solr-cluster-config",
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app": "solr",
			},
		},
		Data: map[string]string{
			"solrHome":    "/store/data",
			"solrPort":    "8983",
			"zkHost":      zookeeperConnect,
			"solrLogsDir": "/store/logs",
			"solrHeap":    "1g",
		},
	}

	if err := ctrl.SetControllerReference(v, configMap, r.Scheme); err != nil {
		return nil, err
	}

	return configMap, nil
}

func (r *SolrReconciler) serviceHeadlessForSolr(v *bigdatav1alpha1.Solr) (*corev1.Service, error) {

	labels := labels(v, "solr")
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "solrcluster",
			Namespace: v.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name: "solr-port",
					Port: 8983,
				},
				{
					Name: "solr-metrics",
					Port: 9983,
				},
			},
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
		},
	}

	if err := ctrl.SetControllerReference(v, s, r.Scheme); err != nil {
		return nil, err
	}

	return s, nil
}

func (r *SolrReconciler) serviceForSolr(v *bigdatav1alpha1.Solr) (*corev1.Service, error) {

	labels := labels(v, "solr")
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "solr-svc",
			Namespace: v.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:     "solr-port",
					Port:     8983,
					NodePort: 30001,
				},
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}

	if err := ctrl.SetControllerReference(v, s, r.Scheme); err != nil {
		return nil, err
	}

	return s, nil
}

// stateFulSetForSolr returns a Solr StateFulSet object
func (r *SolrReconciler) stateFulSetForSolr(solr *bigdatav1alpha1.Solr) (*appsv1.StatefulSet, error) {

	//ls := labelsForSolr(solr.Name)
	labels := labels(solr, "solr")

	replicas := solr.Spec.Size

	// Get the Operand image
	image, err := imageForSolr()
	if err != nil {
		return nil, err
	}

	fastdisks := "fast-disks"

	dep := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      solr.Name,
			Namespace: solr.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "solrcluster",
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: nil,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Image:           image,
							Name:            "solr",
							ImagePullPolicy: corev1.PullAlways,
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 8983,
									Name:          "solr-port",
								},
								{
									ContainerPort: 9983,
									Name:          "solr-metrics",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "MY_POD_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name: "MY_POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
								{
									Name: "MY_POD_IP",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "status.podIP",
										},
									},
								},
								{
									Name: "SOLR_HOME",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "solrHome",
										},
									},
								},
								{
									Name: "ZK_HOST",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "zkHost",
										},
									},
								},
								{
									Name: "POD_HOSTNAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{
									Name:  "SOLR_HOST",
									Value: "$(POD_HOSTNAME).solrcluster",
								},
								{
									Name: "SOLR_LOGS_DIR",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "solrLogsDir",
										},
									},
								},
								{
									Name: "SOLR_HEAP",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "solrHeap",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "volsolr",
									MountPath: "/store",
								},
							},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:            "init-solr-data",
							Image:           "busybox",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/bin/sh",
								"-c",
								"if [ ! -d $SOLR_HOME/lib ] ; then mkdir -p $SOLR_HOME/lib && chown -R 8983:8983 $SOLR_HOME && chmod -R 777 $SOLR_HOME; else chmod -R 777 $SOLR_HOME; fi",
							},
							Env: []corev1.EnvVar{
								{
									Name: "SOLR_HOME",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "solrHome",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "volsolr",
									MountPath: "/store",
								},
							},
						},
						{
							Name:            "init-solr-logs",
							Image:           "busybox",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/bin/sh",
								"-c",
								"if [ ! -d $SOLR_LOGS_DIR ] ; then mkdir -p $SOLR_LOGS_DIR && chmod -R 777 $SOLR_LOGS_DIR && chown 8983:8983 $SOLR_LOGS_DIR ; else chmod -R 777 $SOLR_LOGS_DIR; fi",
							},
							Env: []corev1.EnvVar{
								{
									Name: "SOLR_LOGS_DIR",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "solrLogsDir",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "volsolr",
									MountPath: "/store",
								},
							},
						},
						{
							Name:            "init-solr-xml",
							Image:           "solr:8.1.1",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/bin/sh",
								"-c",
								"if [ ! -f $SOLR_HOME/solr.xml ] ; then cp /opt/solr/server/solr/solr.xml $SOLR_HOME/solr.xml; sed -i \"s/<solr>/<solr><str name='sharedLib'>\\/store\\/data\\/lib<\\/str>/g\" $SOLR_HOME/solr.xml ; else true; fi ",
							},
							Env: []corev1.EnvVar{
								{
									Name: "SOLR_HOME",
									ValueFrom: &corev1.EnvVarSource{
										ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "solr-cluster-config",
											},
											Key: "solrHome",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "volsolr",
									MountPath: "/store",
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "volsolr",
					Labels: labels,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("150Mi"),
						},
					},
					StorageClassName: &fastdisks,
				},
			}},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(solr, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// labelsForSolr returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForSolr(name string) map[string]string {
	var imageTag string
	image, err := imageForSolr()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "Solr",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "solr-operator",
		"app.kubernetes.io/created-by": "controller-manager",
	}
}

// imageForSolr gets the Operand image which is managed by this controller
// from the SOLR_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForSolr() (string, error) {
	/*var imageEnvVar = "SOLR_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find %s environment variable with the image", imageEnvVar)
	}*/
	image := "docker.io/kubernetesbigdataeg/solr-alpine:8.11.1"
	return image, nil
}

func labels(v *bigdatav1alpha1.Solr, l string) map[string]string {
	return map[string]string{
		"app": l,
	}
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *SolrReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bigdatav1alpha1.Solr{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
