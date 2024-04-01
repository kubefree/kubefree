package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/qiniu/x/log"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

func (c *controller) checkNamespace(ns *v1.Namespace) error {
	ac, ok := ns.Annotations[c.ActivityStatusAnnotation]
	if !ok || ac == "" {
		// activity status not exists, do nothing
		return nil
	}

	lastActivityStatus, err := getActivity(ac)
	if err != nil {
		log.Errorf("Error getActivity: %v", ac)
		return err
	}

	nc := namespaceController{controller: *c}

	// check delete-after-seconds rules
	if deleted, err := nc.syncDeleteAfterRules(ns, *lastActivityStatus); err != nil {
		log.Errorf("Error SyncDeleteAfterRules: %v", err)
		return err
	} else if deleted {
		// if namespace is deleted, no need to check sleep-after rules
		return nil
	}

	// check sleep-after rules
	if err = nc.syncSleepAfterRules(ns, *lastActivityStatus); err != nil {
		log.Errorf("Error SyncSleepAfterRules: %v", err)
		return err
	}

	return nil
}

type namespaceController struct {
	controller
}

// target delete-after rules
func (c *namespaceController) syncDeleteAfterRules(namespace *v1.Namespace, lastActivity activity) (deleted bool, err error) {
	v, ok := namespace.Labels[c.DeleteAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have delete-after label, do nothing
		return
	}

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return false, fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration && namespace.Status.Phase != v1.NamespaceTerminating {
		if !c.DryRun {
			if err != c.clientset.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{}) {
				log.Errorf("Error delete namespace: %v", err)
				return false, err
			}

			log.Info("delete inactivity namespace success")
			return true, nil
		}
	}
	return
}

// target sleep-after rules
func (c *namespaceController) syncSleepAfterRules(namespace *v1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.SleepAfterSelector, v)
	}

	state := namespace.Labels[c.ExecutionStateSelector]
	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		switch state {
		case DELETING:
			// do nothing
		case SLEEP, SLEEPING:
			// delete namespace if the namespace still in inactivity status after thresholdDuration * 2 time
			if namespace.Status.Phase != v1.NamespaceTerminating && time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration*2 {
				if !c.DryRun {
					if err := c.clientset.CoreV1().Namespaces().Delete(context.TODO(), namespace.Name, metav1.DeleteOptions{}); err != nil {
						log.Errorf("Error delete namespace: %v", err)
						return err
					}
					log.Info("delete inactivity namespace success")
				}
			}
		default:
			/*
				Enter into sleep mode
				step1: update execution state with sleeping
				step2: sleep namespace
				step3: update execution state with sleep
			*/
			log.Info("try to sleep inactivity namespace")
			if _, err := c.setKubefreeExecutionState(namespace, SLEEPING); err != nil {
				log.Errorf("failed to SetKubefreeExecutionState: %v", err)
				return err
			}
			if !c.DryRun {
				if err := c.sleep(namespace); err != nil {
					log.Errorf("error sleep namespace: %v", err)
					return err
				}
			}
			if _, err := c.setKubefreeExecutionState(namespace, SLEEP); err != nil {
				log.Errorf("failed to SetKubefreeExecutionState: %v", err)
				return err
			}
			log.Info("sleep inactivity namespace successfully")
		}
	} else {
		switch state {
		case SLEEPING, SLEEP:
			log.Infof("try to wake up namespace %s", namespace.Name)
			if !c.DryRun {
				if err := c.wakeUp(namespace); err != nil {
					log.Errorf("Error wake up namespace: %v", err)
					return err
				}
				log.Info("wake up namespace successfully")
			}

			if _, err := c.setKubefreeExecutionState(namespace, NORMAL); err != nil {
				log.Errorf("failed to SetKubefreeExecutionState: %v", err)
			}

		default:
			// still in activity time scope,so do nothing
		}
	}
	return nil
}

func (c *namespaceController) setKubefreeExecutionState(namespace *v1.Namespace, state string) (*v1.Namespace, error) {
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

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNsData, newNsData, &v1.Namespace{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.CoreV1().Namespaces().Patch(context.TODO(), namespace.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Errorf("failed to patch annotation for namespace, namespace: %v , err: %v", namespace.Name, err)
		return nil, err
	}
	return result, nil
}

// 依次判断deployment、statefulset、deamonset，每个执行以下操作
//  1. 判断是否有legacy replicas，如果有则报错
//  2. 获得原来的replicas，并保存为legacy replicas annotation
//  3. 设置 replicas ==0
func (c *namespaceController) sleep(ns *v1.Namespace) error {
	//TODO: refactor
	// deployment
	deploymentLists, err := c.clientset.AppsV1().Deployments(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments, with err %v", err)
	}

	for _, d := range deploymentLists.Items {
		_, err = c.patchDeploymentWithAnnotation(&d, LegacyReplicasAnnotation, strconv.FormatInt(int64(*d.Spec.Replicas), 10))
		if err != nil {
			return fmt.Errorf("failed to set annotation for deployment %s, err: %v", d.Name, err)
		}

		_, err = c.scale(context.TODO(), d.Name, d.Namespace, "0", schema.GroupResource{Group: "apps", Resource: "deployments"})
		if err != nil {
			return fmt.Errorf("failed to update scale for deployment %s/%s, with err %v", d.Namespace, d.Name, err)
		}

		log.Infof("sleep deployment %s successfully", d.Name)
	}

	// statefulset
	ssLists, err := c.clientset.AppsV1().StatefulSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list StatefulSets, with err %v", err)
	}

	for _, ss := range ssLists.Items {
		_, err = c.patchStatefulsetWithAnnotation(&ss, LegacyReplicasAnnotation, strconv.FormatInt(int64(*ss.Spec.Replicas), 10))
		if err != nil {
			return fmt.Errorf("failed to set annotation for statefulset %s, err: %v", ss.Name, err)
		}

		_, err = c.scale(context.TODO(), ss.Name, ss.Namespace, "0", schema.GroupResource{Group: "apps", Resource: "statefulsets"})
		if err != nil {
			return fmt.Errorf("failed to update scale for statefulset %s/%s, with err %v", ss.Namespace, ss.Name, err)
		}
		log.Infof("sleep statefulset %s successfully", ss.Name)
	}

	// deamonsets
	dsLists, err := c.clientset.AppsV1().DaemonSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list daemonset, with err %v", err)
	}

	for _, ss := range dsLists.Items {
		_, err = c.patchDaemonsetWithNodeAffinity(&ss, LegacyReplicasAnnotation, strconv.FormatInt(int64(ss.Status.DesiredNumberScheduled), 10))
		if err != nil {
			return fmt.Errorf("failed to set annotation for daemonset %s, err: %v", ss.Name, err)
		}

		log.Infof("sleep daemonsets %v successfully", ss.Name)
	}

	// TODO: others?

	return nil
}

// TODO: is it necessary to delete legacy replicas annotation?
func (c *namespaceController) wakeUp(ns *v1.Namespace) error {
	// 依次判断deployment、statefulset、deamonset，每个执行以下操作
	// 	1. 判断是否有legacy replicas，如果没有则报错
	//  2. 设置 replicas == legacy replicas

	workloadWg := &sync.WaitGroup{}

	// error channel to collect failures.
	// will make the buffer big enough to avoid any blocking
	var errCh chan error

	// deployment
	deploymentLists, err := c.clientset.AppsV1().Deployments(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments, with err %v", err)
	}

	// TODO: can deploymentLists be nil?
	if deploymentLists != nil {
		workloadWg.Add(len(deploymentLists.Items))
		errCh = make(chan error, len(deploymentLists.Items))
	}

	// statefulset
	ssLists, err := c.clientset.AppsV1().StatefulSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list StatefulSets, with err %v", err)
	}
	// TODO: can ssLists be nil?
	if ssLists != nil {
		workloadWg.Add(len(ssLists.Items))
		errCh = make(chan error, len(errCh)+len(ssLists.Items))
	}

	// deamonsets
	dsLists, err := c.clientset.AppsV1().DaemonSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list DaemonSets, with err %v", err)
	}

	if dsLists != nil {
		workloadWg.Add(len(dsLists.Items))
		errCh = make(chan error, len(errCh)+len(dsLists.Items))
	}

	for _, dsItem := range dsLists.Items {
		v, ok := dsItem.Annotations[LegacyReplicasAnnotation]
		if !ok || len(v) == 0 {
			return fmt.Errorf("unexpected legacy replicas annotation %s for the daemonsets %s", LegacyReplicasAnnotation, dsItem.Name)
		}
		go func(ds apps.DaemonSet) {
			defer workloadWg.Done()
			_, err = c.patchDaemonsetWithoutSpecificNodeAffinity(&ds, LegacyReplicasAnnotation, strconv.FormatInt(int64(ds.Status.DesiredNumberScheduled), 10))
			if err != nil {
				log.Errorf("failed to set annotation for daemonset %s, err: %v", ds.Name, err)
				errCh <- err
				utilruntime.HandleError(err)
			}

		}(dsItem)
	}

	for _, deploy := range deploymentLists.Items {
		v, ok := deploy.Annotations[LegacyReplicasAnnotation]
		if !ok || len(v) == 0 {
			return fmt.Errorf("unexpected legacy replicas annotation %s for the deployment %s", LegacyReplicasAnnotation, deploy.Name)
		}

		go func(d apps.Deployment) {
			defer workloadWg.Done()
			_, err = c.scale(context.TODO(), d.Name, d.Namespace, v, schema.GroupResource{Group: "apps", Resource: "deployments"})
			if err != nil {
				log.Errorf("failed to update scale for deployment %s/%s, with err %v", d.Namespace, d.Name, err)
				errCh <- err
				utilruntime.HandleError(err)
			}
		}(deploy)
	}

	for _, statefulsetItem := range ssLists.Items {
		v, ok := statefulsetItem.Annotations[LegacyReplicasAnnotation]
		if !ok || len(v) == 0 {
			return fmt.Errorf("unexpected legacy replicas annotation %s for the deployment %s", LegacyReplicasAnnotation, statefulsetItem.Name)
		}

		go func(ss apps.StatefulSet) {
			defer workloadWg.Done()
			_, err = c.scale(context.TODO(), ss.Name, ss.Namespace, v, schema.GroupResource{Group: "apps", Resource: "statefulsets"})
			if err != nil {
				log.Errorf("failed to update scale for statefulset %s/%s, with err %v", ss.Namespace, ss.Name, err)
				errCh <- err
				utilruntime.HandleError(err)
			}
		}(statefulsetItem)
	}

	workloadWg.Wait()
	// collect errors if any for proper reporting/retry logic in the controller
	errors := []error{}
	close(errCh)
	for err := range errCh {
		errors = append(errors, err)
	}
	return utilerrors.NewAggregate(errors)
}
