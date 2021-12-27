package plank

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/klog/v2"
)

type controller struct {
	clientset *kubernetes.Clientset
	sleeper   Sleeper

	DeleteAfterSelector      string
	SleepAfterSelector       string
	ActivityStatusAnnotation string
	ExecutionStateSelector   string
}

func NewController(clientset *kubernetes.Clientset) (*controller, error) {
	cachedDiscoveryClient := cacheddiscovery.NewMemCacheClient(clientset.DiscoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(clientset.DiscoveryClient)
	scaler := scale.New(clientset.RESTClient(), mapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)

	return &controller{
		clientset:                clientset,
		sleeper:                  NewSleeper(scaler, clientset),
		DeleteAfterSelector:      DeleteAfterSecondsLabel,
		SleepAfterSelector:       SleepAfterSecondsLabel,
		ActivityStatusAnnotation: NamespaceActivityStatusAnnotation,
		ExecutionStateSelector:   ExecutionStateLabel,
	}, nil
}

func (c *controller) SyncLoop(ctx context.Context) {
	// TODO: 没必要先等待10秒
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// TODO: 异步执行
			c.sync()
		}
	}
}

func (c *controller) sync() {
	// TODO: 无脑拉所有 vs 基于Label来list
	// TODO: inform 模式
	nsList, err := c.clientset.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		logrus.WithError(err).Fatalln("Error list namespaces")
	}

	logrus.WithField("count", len(nsList.Items)).Info("total namespaces")
	for _, ns := range nsList.Items {
		ac, ok := ns.Annotations[c.ActivityStatusAnnotation]
		if !ok || ac == "" {
			// activity status not exists
			continue
		}
		lastActivityStatus, err := getActivity(ac)
		if err != nil {
			logrus.WithError(err).Fatalln("Error getActivity")
		}

		// check delete-after-seconds rules
		if err = c.SyncDeleteAfterRules(ns, *lastActivityStatus); err != nil {
			logrus.WithError(err).Fatalln("Error SyncDeleteAfterRules")
		}

		// check sleep-after rules
		if err = c.SyncSleepAfterRules(ns, *lastActivityStatus); err != nil {
			logrus.WithError(err).Fatalln("Error SyncSleepAfterRules")
		}
	}
	// TODO: 统计每一轮的用时，最好用统一的Reqid来标记，方便后续排查问题
	logrus.Info("End of sync all namespaces")
}

// target sleep-after rules
func (c *controller) SyncSleepAfterRules(namespace corev1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}
	logrus.WithField("namespace", namespace.Name).WithField("sleep-after", v).Debug("check it's sleep-after rule")

	threshold, err := strconv.Atoi(v)
	if err != nil {
		return err
	}

	state := namespace.Labels[c.ExecutionStateSelector]
	if time.Since(lastActivity.LastActivityTime) > time.Duration(threshold)*time.Second {
		switch state {
		case DELETING:
			// do nothing
			// TODO: how to deal it if namespace hangs?
		case SLEEP, SLEEPING:
			// delete namespace if the namespace still in inactivity status
			if namespace.Status.Phase != corev1.NamespaceTerminating && time.Since(lastActivity.LastActivityTime)*2 > time.Duration(threshold)*3 {
				err = c.clientset.CoreV1().Namespaces().Delete(context.TODO(), namespace.Name, metav1.DeleteOptions{})
				if err != nil {
					logrus.WithField("namespace", namespace.Name).Error("Error delete namespace")
					return err
				}
				logrus.WithField("namespace", namespace.Name).WithField("sleep-after", v).Info("delete inactivity namespace")
			}
		default:
			/*
				Enter into sleep mode
				step1: update execution state with sleeping
				step2: sleep namespace
				step3: update execution state with sleep
			*/
			if _, err := c.SetKubefreeExecutionState(namespace, SLEEPING); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to SetKubefreeExecutionState")
			}
			if err := c.sleeper.Sleep(namespace); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to sleep namespace")
			}
			if _, err := c.SetKubefreeExecutionState(namespace, SLEEP); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to SetKubefreeExecutionState")
			}
		}
	} else {
		switch state {
		case SLEEPING, SLEEP:
			// TODO: do recovery
			// 这块需要考虑服务的启动顺序。
			// 也许并发的恢复所有的服务，出错的概率会小？
			// klog.Info("recovery namespace", namespace.Name)
			// if _, err := c.SetKubefreeExecutionState(namespace, NORMAL); err != nil {
			// 	logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to SetKubefreeExecutionState")
			// }
		default:
			// still in activity time scope,so do nothing
		}
	}
	return nil
}

// target delete-after rules
func (c *controller) SyncDeleteAfterRules(namespace corev1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.DeleteAfterSelector]
	if !ok || v != "" {
		// namespace doesn't have delete-after label, do nothing
		return nil
	}
	logrus.WithField("namespace", namespace.Name).WithField("delete-after", v).Debug("check it's delete-after rule")

	thresholdSeconds, err := strconv.Atoi(v)
	if err != nil {
		return err
	}

	// 判断
	if time.Since(lastActivity.LastActivityTime) > time.Duration(thresholdSeconds) && namespace.Status.Phase != corev1.NamespaceTerminating {
		logrus.WithField("namespace", namespace.Name).Info("deleting inactivity namespace")
		if err != c.clientset.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{}) {
			logrus.WithField("namespace", namespace.Name).Error("Error delete namespace")
			return err
		}
	}
	return nil
}

func (c *controller) SetKubefreeExecutionState(namespace corev1.Namespace, state string) (*corev1.Namespace, error) {
	ns, err := c.clientset.CoreV1().Namespaces().Get(context.TODO(), namespace.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	oldNsData, err := json.Marshal(ns)
	if err != nil {
		return nil, err
	}

	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	ns.Labels[c.ExecutionStateSelector] = state
	newNsData, err := json.Marshal(ns)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNsData, newNsData, &corev1.Namespace{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.CoreV1().Namespaces().Patch(context.TODO(), namespace.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.V(2).InfoS("failed to patch annotation for namespace", "namespace", namespace.Name, "err", err)
		return nil, err
	}
	return result, nil
}
