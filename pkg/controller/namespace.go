package plank

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/klog/v2"
)

const (
	SleepAfterLabel     = "sleepmode.kubefree.com/sleep-after"
	DeleteAfterLabel    = "sleepmode.kubefree.com/delete-after"
	ExecutionStateLabel = "sleepmode.kubefree.com/state"

	NamespaceActivityStatusAnnotation = "sleepmode.kubefree.com/activity-status"
	LegacyReplicasAnnotation          = "sleepmode.kubefree.com/legacy-replicas"
)

// execution state of namespace controlled by kubefree
const (
	NORMAL   string = "normal"
	SLEEPING string = "sleeping"
	SLEEP    string = "sleep"
	DELETING string = "deleting"
)

type userInfo struct {
	Name        string `json:"Name"`
	Impersonate string `json:"Impersonate"`
	RancherName string `json:"RancherName, omitempty"`
}
type activity struct {
	LastActivityTime time.Time `json:"LastActivityTime"`
	Action           string    `json:"Action"`
	Resource         string    `json:"Resource"`
	Namespace        string    `json:"Namespace"`
	User             userInfo  `json:"UserInfo"`
}

func getActivity(src string) (*activity, error) {
	activity := &activity{}
	err := json.Unmarshal([]byte(src), activity)
	if err != nil {
		return nil, err
	}
	return activity, nil
}

func (c *controller) validDeploymentAnnotation(d *apps.Deployment, annotation, value string) (*apps.Deployment, error) {
	return nil, nil
}

func (c *controller) patchDeploymentWithAnnotation(d *apps.Deployment, annotation, value string) (*apps.Deployment, error) {
	if d == nil {
		return nil, fmt.Errorf("unexpected deployment as nil")
	}
	oldDeploymentData, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}

	newD := d.DeepCopy()
	if newD.Annotations == nil {
		newD.Annotations = make(map[string]string)
	}

	newD.Annotations[annotation] = value
	newDeploymentData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldDeploymentData, newDeploymentData, &apps.Deployment{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().Deployments(d.Namespace).Patch(context.TODO(), d.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.V(2).InfoS("failed to patch annotation for deployment", "deployment", d.Name, "err", err)
		return nil, err
	}
	return result, nil
}

func (c *controller) patchStatefulsetWithAnnotation(s *apps.StatefulSet, annotation, value string) (*apps.StatefulSet, error) {
	if s == nil {
		return nil, fmt.Errorf("unexpected statefulset as nil")
	}

	oldData, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	newD := s.DeepCopy()
	if newD.Annotations == nil {
		newD.Annotations = make(map[string]string)
	}

	newD.Annotations[annotation] = value
	newData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &apps.StatefulSet{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().StatefulSets(s.Namespace).Patch(context.TODO(), s.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.V(2).InfoS("failed to patch annotation for statefulset", "statefulset", s.Name, "err", err)
		return nil, err
	}
	return result, nil
}
