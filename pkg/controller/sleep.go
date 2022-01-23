package plank

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// method to scale workload's replica to 0 or recovery
type Sleeper interface {
	Sleep(ns *v1.Namespace) error
	WakeUp(ns *v1.Namespace) error
}

func (c *controller) Sleep(ns *v1.Namespace) error {
	// 依次判断deployment、statefulset、deamonset，每个执行以下操作
	// 	1. 判断是否有legacy replicas，如果有则报错
	// 	2. 获得原来的replicas，并保存为legacy replicas annotation
	//  3. 设置 replicas ==0

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

		logrus.WithField("namespace", ns.Name).WithField("deployment", d.Name).Info("sleep deployment successfully")
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
		logrus.WithField("namespace", ns.Name).WithField("statefulset", ss.Name).Info("sleep statefulset successfully")
	}

	// deamonsets
	dsLists, err := c.clientset.AppsV1().DaemonSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list DaemonSets, with err %v", err)
	}

	for _, ss := range dsLists.Items {
		_, err = c.patchDaemonsetWithNodeAffinity(&ss, LegacyReplicasAnnotation, strconv.FormatInt(int64(ss.Status.DesiredNumberScheduled), 10))
		if err != nil {
			return fmt.Errorf("failed to set annotation for statefulset %s, err: %v", ss.Name, err)
		}

		logrus.WithField("namespace", ns.Name).WithField("statefulset", ss.Name).Info("sleep statefulset successfully")
	}

	// TODO: others?

	return nil
}

//TODO: is it necessary to delete legacy replicas annotation?
func (c *controller) WakeUp(ns *v1.Namespace) error {
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
			return fmt.Errorf("unexpected legacy replicas annotation %s for the daemonset %s", LegacyReplicasAnnotation, dsItem.Name)
		}
		go func(ds apps.DaemonSet) {
			defer workloadWg.Done()
			_, err = c.patchDaemonsetWithoutSpecificNodeAffinity(&ds, LegacyReplicasAnnotation, strconv.FormatInt(int64(ds.Status.DesiredNumberScheduled), 10))
			if err != nil {
				klog.V(2).Infof("Failed scaled for daemonset %q/%q", ds.Namespace, &ds.Name)
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
				klog.V(2).Infof("Failed scaled for deployment %q/%q", d.Namespace, d.Name)
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
				klog.V(2).Infof("Failed scaled for deployment %q/%q", ss.Namespace, ss.Name)
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

func (c *controller) scale(ctx context.Context, name, namespace, scale string, resource schema.GroupResource) (*autoscalingv1.Scale, error) {
	i, err := strconv.ParseInt(scale, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("strconv.ParseInt failed for scale %s/%s", namespace, name)
	}

	targetScale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: int32(i),
		},
	}

	return c.scalesGetter.Scales(namespace).Update(ctx, resource, targetScale, metav1.UpdateOptions{})
}
