/*
   Copyright @ 2021 bocloud <fushaosong@beyondcent.com>.

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
	"encoding/json"
	"fmt"
	"github.com/carina-io/carina/pkg/configuration"
	"github.com/carina-io/carina/utils"
	"github.com/carina-io/carina/utils/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"strings"
	"time"
)

// PersistentVolumeClaimReconciler reconciles a PersistentVolumeClaim object
type PersistentVolumeReconciler struct {
	client.Client
	APIReader      client.Reader
	cacheConfigMap map[string]map[string]string
	lastEventTime  time.Time
}

// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

// Reconcile finalize PVC
func (r *PersistentVolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	if time.Now().Sub(r.lastEventTime).Seconds() < 15 {
		return ctrl.Result{}, nil
	}

	r.lastEventTime = time.Now()
	time.Sleep(time.Duration(rand.Int63nRange(30, 60)) * time.Second)
	r.lastEventTime = time.Now()
	// your logic here
	pv := &corev1.PersistentVolume{}
	err := r.Get(ctx, req.NamespacedName, pv)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Errorf("unable to fetch persistentvolume %s, %s", req.Name, err.Error())
			return ctrl.Result{}, err
		}
	} else {
		if pv.Spec.CSI == nil {
			return ctrl.Result{}, nil
		}
		if pv.Spec.CSI.Driver != utils.CSIPluginName {
			return ctrl.Result{}, nil
		}
	}

	err = r.updateNodeConfigMap(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up Reconciler with Manager.
func (r *PersistentVolumeReconciler) SetupWithManager(mgr ctrl.Manager, stopChan <-chan struct{}) error {

	r.cacheConfigMap = make(map[string]map[string]string)
	r.lastEventTime = time.Now()

	ticker1 := time.NewTicker(60 * time.Second)
	go func(t *time.Ticker) {
		defer ticker1.Stop()
		//after := time.After(300 * time.Second)
		for {
			select {
			case <-t.C:
				err := r.updateNodeConfigMap(context.Background())
				if err != nil {
					log.Errorf("update node storage config map failed %s", err.Error())
				}
			case <-stopChan:
				log.Info("graceful stop config map sync goroutine")
				return
				//case <-after:
				//	log.Info("stop node storage config map update...")
				//	return
			}
		}
	}(ticker1)

	pred := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(pred).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewItemFastSlowRateLimiter(10*time.Second, 60*time.Second, 5),
		}).
		For(&corev1.PersistentVolume{}).
		Complete(r)
}

func (r *PersistentVolumeReconciler) updateNodeConfigMap(ctx context.Context) error {
	nl := new(corev1.NodeList)
	err := r.List(ctx, nl)
	if err != nil {
		return err
	}

	nodeDevice := []map[string]string{}
	for _, node := range nl.Items {
		tmp := map[string]string{}
		for key, v := range node.Status.Capacity {
			if strings.HasPrefix(string(key), utils.DeviceCapacityKeyPrefix) {
				tmp["capacity."+string(key)] = fmt.Sprintf("%d", v.Value())
			}
		}
		for key, v := range node.Status.Allocatable {
			if strings.HasPrefix(string(key), utils.DeviceCapacityKeyPrefix) {
				tmp["allocatable."+string(key)] = fmt.Sprintf("%d", v.Value())
			}
		}
		if len(tmp) > 0 {
			tmp["nodeName"] = node.Name
			nodeDevice = append(nodeDevice, tmp)
		}
	}

	if r.cacheHit(nodeDevice) {
		return nil
	}

	byteJson, err := json.Marshal(nodeDevice)
	if err != nil {
		log.Errorf("carina-node-storage json marshal failed %s", err.Error())
		return err
	}

	cm := &corev1.ConfigMap{}
	err = r.Get(ctx, client.ObjectKey{Namespace: configuration.RuntimeNamespace(), Name: "carina-node-storage"}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c := corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ConfigMap",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "carina-node-storage",
					Namespace: configuration.RuntimeNamespace(),
				},
				Data: map[string]string{"node": string(byteJson)},
			}
			err = r.Create(ctx, &c)
			if err != nil {
				log.Errorf("update config map carina-vg failed %s", err.Error())
				return err
			}
			return nil
		}
		return err
	}

	log.Info("need modify config map carina-node-storage")
	cm.Data = map[string]string{"node": string(byteJson)}
	err = r.Update(ctx, cm)
	if err != nil {
		log.Errorf("update config map carina-vg failed %s", err.Error())
		return err
	}
	r.cacheRefresh(nodeDevice)
	return nil
}

func (r *PersistentVolumeReconciler) cacheHit(nodeDevice []map[string]string) bool {

	if len(r.cacheConfigMap) != len(nodeDevice) {
		return false
	}
	for _, value := range nodeDevice {
		if _, ok := r.cacheConfigMap[value["nodeName"]]; !ok {
			return false
		}

		if !utils.MapEqualMap(value, r.cacheConfigMap[value["nodeName"]]) {
			return false
		}
	}
	return true
}

func (r *PersistentVolumeReconciler) cacheRefresh(nodeDevice []map[string]string) {
	r.cacheConfigMap = make(map[string]map[string]string)
	for _, value := range nodeDevice {
		r.cacheConfigMap[value["nodeName"]] = value
	}
}
