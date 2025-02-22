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
	"bufio"
	"context"
	"fmt"
	"github.com/carina-io/carina/utils"
	"github.com/carina-io/carina/utils/exec"
	"github.com/carina-io/carina/utils/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/workqueue"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"strings"
	"time"
)

const (
	// pod annotation KubernetesCustomized/BlkIOThrottleReadBPS
	KubernetesCustomized   = "kubernetes.customized"
	BlkIOThrottleReadBPS   = "blkio.throttle.read_bps_device"
	BlkIOThrottleReadIOPS  = "blkio.throttle.read_iops_device"
	BlkIOThrottleWriteBPS  = "blkio.throttle.write_bps_device"
	BlkIOThrottleWriteIOPS = "blkio.throttle.write_iops_device"
	BlkIOCGroupPath        = "/sys/fs/cgroup/blkio/"
)

// PodReconciler reconciles a Node object
type PodReconciler struct {
	client.Client
	NodeName string
	Executor exec.Executor
	// stop
	StopChan <-chan struct{}
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;delete

// Reconcile finalize Node
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// your logic here
	pod := &corev1.Pod{}
	err := r.Client.Get(ctx, req.NamespacedName, pod)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, err
	}

	if pod.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	// 等待容器启动成功在设置cgroup
	for i := 0; i < 5; i++ {
		if pod.Status.Phase == corev1.PodRunning {
			break
		}
		time.Sleep(30 * time.Second)
		err := r.Client.Get(ctx, req.NamespacedName, pod)
		if err == nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.SinglePodCGroupConfig(ctx, pod); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up Reconciler with Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {

	ctx := context.Background()
	err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.Pod{}, "combinedIndex", func(object client.Object) []string {
		combinedIndex := fmt.Sprintf("%s-%s", object.(*corev1.Pod).Spec.SchedulerName, object.(*corev1.Pod).Spec.NodeName)
		return []string{combinedIndex}
	})
	if err != nil {
		return err
	}

	err = mgr.GetFieldIndexer().IndexField(ctx, &corev1.PersistentVolume{}, "pvIndex", func(object client.Object) []string {
		pvIndex := []string{}
		pv := object.(*corev1.PersistentVolume)
		if pv == nil {
			return pvIndex
		}
		if pv.Spec.CSI == nil {
			return pvIndex
		}
		if pv.Spec.CSI.Driver != utils.CSIPluginName {
			return pvIndex
		}
		if pv.Spec.ClaimRef == nil {
			return pvIndex
		}
		pvIndex = append(pvIndex, fmt.Sprintf("%s-%s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name))
		return pvIndex
	})
	if err != nil {
		return err
	}

	ticker1 := time.NewTicker(600 * time.Second)
	go func(t *time.Ticker) {
		defer ticker1.Stop()
		for {
			select {
			case <-t.C:
				_ = r.AllPodCGroupConfig(ctx)
			case <-r.StopChan:
				log.Info("stop device monitor...")
				return
			}
		}
	}(ticker1)

	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(podFilter{r.NodeName}).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewItemFastSlowRateLimiter(10*time.Second, 60*time.Second, 5),
		}).
		For(&corev1.Pod{}).
		Complete(r)
}

// 对于单独一个Pod的创建,修改事件只需要判断当前cgroup和pod.annotations对比，即可判定是否需要变更
func (r *PodReconciler) SinglePodCGroupConfig(ctx context.Context, pod *corev1.Pod) error {

	log.Infof("config cgroup blkio %s %s", pod.GetNamespace(), pod.GetName())

	cb := []*cgroupblkio{}
	for _, v := range []string{BlkIOThrottleReadBPS, BlkIOThrottleReadIOPS, BlkIOThrottleWriteBPS, BlkIOThrottleWriteIOPS} {
		cb = append(cb, &cgroupblkio{
			name:     v,
			cpath:    filepath.Join(BlkIOCGroupPath, v),
			oldBlkio: map[string]string{},
			newBlkio: map[string]string{},
		})
	}

	for _, volume := range pod.Spec.Volumes {
		if volume.VolumeSource.PersistentVolumeClaim == nil {
			continue
		}
		pvList := &corev1.PersistentVolumeList{}
		err := r.Client.List(ctx, pvList, client.MatchingFields{"pvIndex": fmt.Sprintf("%s-%s", pod.GetNamespace(), volume.VolumeSource.PersistentVolumeClaim.ClaimName)})
		if err != nil {
			log.Errorf("get pv %s failed %s", volume.VolumeSource.PersistentVolumeClaim.ClaimName, err.Error())
			continue
		}

		if len(pvList.Items) != 1 {
			log.Errorf("get pv count not equal one,  %d", len(pvList.Items))
			continue
		}

		pvInfo := pvList.Items[0]
		if pvInfo.Spec.CSI.Driver != utils.CSIPluginName {
			continue
		}
		// 设置主从版本号作为Key
		blkioKey := fmt.Sprintf("%s:%s", pvInfo.Spec.CSI.VolumeAttributes[utils.VolumeDeviceMajor], pvInfo.Spec.CSI.VolumeAttributes[utils.VolumeDeviceMinor])
		// 填充到将要变更的cgroup
		for _, c := range cb {
			newValue, newOk := pod.Annotations[fmt.Sprintf("%s/%s", KubernetesCustomized, c.name)]
			// 对于单独Pod的更新这里判断很简单，如果存在这个注解则更新，如果不存在这个注解则删除
			if newOk {
				c.newBlkio[blkioKey] = newValue
			} else {
				c.newBlkio[blkioKey] = "0"
			}
		}
	}
	// 变更cgroup file
	writeCgroupBlkioFile(r.Executor, cb)
	return nil
}

func (r *PodReconciler) AllPodCGroupConfig(ctx context.Context) error {
	log.Info("config all pod cgroup blkio")

	podList := &corev1.PodList{}
	err := r.Client.List(ctx, podList, client.MatchingFields{"combinedIndex": fmt.Sprintf("%s-%s", utils.CarinaSchedule, r.NodeName)})
	if err != nil {
		return err
	}
	// 获取当前cgroup 配置
	cb := readCGroupBlkioFile()
	// 获取设备限制
	for _, p := range podList.Items {
		for _, volume := range p.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				continue
			}
			pvList := &corev1.PersistentVolumeList{}
			err := r.Client.List(ctx, pvList, client.MatchingFields{"pvIndex": fmt.Sprintf("%s-%s", p.GetNamespace(), volume.VolumeSource.PersistentVolumeClaim.ClaimName)})
			if err != nil {
				log.Errorf("get pv %s failed %s", volume.VolumeSource.PersistentVolumeClaim.ClaimName, err.Error())
				continue
			}
			if len(pvList.Items) != 1 {
				log.Errorf("get pv count not equal one,  %d", len(pvList.Items))
				continue
			}
			pvInfo := pvList.Items[0]
			if pvInfo.Spec.CSI.Driver != utils.CSIPluginName {
				continue
			}

			// 设置主从版本号作为Key
			blkioKey := fmt.Sprintf("%s:%s", pvInfo.Spec.CSI.VolumeAttributes[utils.VolumeDeviceMajor], pvInfo.Spec.CSI.VolumeAttributes[utils.VolumeDeviceMinor])
			// 填充到将要变更的cgroup
			for _, c := range cb {
				_, oldOk := c.oldBlkio[blkioKey]
				newValue, newOk := p.Annotations[fmt.Sprintf("%s/%s", KubernetesCustomized, c.name)]
				if newOk {
					c.newBlkio[blkioKey] = newValue
				} else {
					if oldOk {
						c.newBlkio[blkioKey] = "0"
					}
				}
			}
		}
	}
	// 判断设备是否需要更新
	writeCgroupBlkioFile(r.Executor, cb)
	return nil
}

// filter carina pod
type podFilter struct {
	nodeName string
}

func (p podFilter) filter(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.Spec.NodeName == p.nodeName && pod.Spec.SchedulerName == utils.CarinaSchedule {
		return true
	}
	return false
}

func (p podFilter) Create(e event.CreateEvent) bool {
	return p.filter(e.Object.(*corev1.Pod))
}

func (p podFilter) Delete(e event.DeleteEvent) bool {
	return false
}

func (p podFilter) Update(e event.UpdateEvent) bool {
	return p.filter(e.ObjectNew.(*corev1.Pod))
}

func (p podFilter) Generic(e event.GenericEvent) bool {
	return false
}

type cgroupblkio struct {
	name     string
	cpath    string
	oldBlkio map[string]string
	newBlkio map[string]string
}

func readCGroupBlkioFile() []*cgroupblkio {

	cb := []*cgroupblkio{}
	for _, v := range []string{BlkIOThrottleReadBPS, BlkIOThrottleReadIOPS, BlkIOThrottleWriteBPS, BlkIOThrottleWriteIOPS} {
		cpath := filepath.Join(BlkIOCGroupPath, v)
		ctmp := &cgroupblkio{
			name:     v,
			cpath:    cpath,
			oldBlkio: map[string]string{},
			newBlkio: map[string]string{},
		}
		f, err := os.Open(cpath)
		defer f.Close()
		if err != nil {
			log.Errorf("open file %s error %s", cpath, err.Error())
			continue
		}
		buf := bufio.NewScanner(f)
		for {
			if !buf.Scan() {
				break
			}
			line := buf.Text()
			line = strings.TrimSpace(line)
			strSlice := strings.Split(line, " ")
			ctmp.oldBlkio[strSlice[0]] = strSlice[1]
		}
		cb = append(cb, ctmp)
	}
	return cb
}

// echo 1:2 1 > blkio cgroup 比较神奇的地方，会搜索当前系统是否存在该设备
// echo 1:2 1 > xxx/blkio_throttle_read_bps 当设备不存在时会追加，当存在时会更新
// echo 1:2 0 > xxx/blkio_throttle_read_bps 会删除符合条件的设备
// 除非明确的要删除设备限制，否则不删除
func writeCgroupBlkioFile(exec exec.Executor, cp []*cgroupblkio) {

	for _, c := range cp {
		// 处理一下需要更新的内容
		for k, v := range c.oldBlkio {
			if nv, ok := c.newBlkio[k]; ok {
				if v == nv {
					delete(c.newBlkio, k)
				}
			}
		}
		for k, v := range c.newBlkio {
			err := exec.ExecuteCommand("bash", "-c", fmt.Sprintf("echo %s %s > %s", k, v, c.cpath))
			if err != nil {
				log.Errorf("failed to exec %s error %s", fmt.Sprintf("echo %s %s > %s", k, v, c.cpath), err.Error())
			}
		}
	}
}
