package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
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
	//
	NORMAL   string = "normal"
	SLEEPING string = "sleeping"
	SLEEP    string = "sleep"
	DELETING string = "deleting"
)

func (c *controller) checkNamespace(ns *v1.Namespace) error {
	ac, ok := ns.Annotations[c.ActivityStatusAnnotation]
	if !ok || ac == "" {
		// activity status not exists, do nothing
		return nil
	}

	lastActivityStatus, err := getActivity(ac)
	if err != nil {
		logrus.WithError(err).Errorf("Error getActivity: %v", ac)
		return err
	}

	nc := namespaceController{controller: *c}

	// check delete-after-seconds rules
	if err = nc.syncDeleteAfterRules(ns, *lastActivityStatus); err != nil {
		logrus.WithError(err).Errorln("Error SyncDeleteAfterRules")
		return err
	}

	// check sleep-after rules
	if err = nc.syncSleepAfterRules(ns, *lastActivityStatus); err != nil {
		logrus.WithError(err).Errorln("Error SyncSleepAfterRules")
		return err
	}

	return nil
}

type userInfo struct {
	Name        string `json:"Name"`
	Impersonate string `json:"Impersonate"`
	RancherName string `json:"RancherName,omitempty"`
}

type customTime time.Time

func (ct *customTime) UnmarshalJSON(b []byte) error {
	t, err := time.Parse(`"2006-01-02T15:04:05Z07:00"`, string(b))
	if err == nil {
		*ct = customTime(t)
		return nil
	}

	t, err = time.Parse(`"2006-01-02T15:04:05"`, string(b))
	if err != nil {
		return err
	}

	*ct = customTime(t.UTC())
	return nil
}

func (ct customTime) String() string {
	return time.Time(ct).String()
}

type activity struct {
	LastActivityTime customTime `json:"LastActivityTime"`
	Action           string     `json:"Action"`
	Resource         string     `json:"Resource"`
	Namespace        string     `json:"Namespace"`
	User             userInfo   `json:"UserInfo"`
}

func (a activity) String() string {
	return fmt.Sprintf("LastActivityTime: %v, Action: %v, Resource: %v, Namespace: %v, UserInfo: %v", a.LastActivityTime, a.Action, a.Resource, a.Namespace, a.User)
}

func getActivity(src string) (*activity, error) {
	activity := &activity{}
	err := json.Unmarshal([]byte(src), activity)
	if err != nil {
		return nil, err
	}
	return activity, nil
}

func (ct *customTime) Time() time.Time {
	return time.Time(*ct)
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
		logrus.Errorf("failed to patch annotation for deployment, deployment: %v, err: %v ", d.Name, err)
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
		logrus.Errorf("failed to patch annotation for statefulset, statefulset: %v, err: %v ", s.Name, err)
		return nil, err
	}
	return result, nil
}

// hardcode flag for sleep daemonset
var defaultExpression = v1.NodeSelectorRequirement{
	Key:      "sleep.kubefree.com/not-existing-key",
	Operator: v1.NodeSelectorOpIn,
	Values:   []string{"sleep"},
}

func (c *controller) patchDaemonsetWithNodeAffinity(s *apps.DaemonSet, annotation, value string) (*apps.DaemonSet, error) {
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

	// still patch daemonset with legacy-replicas annotation
	newD.Annotations[annotation] = value

	if newD.Spec.Template.Spec.Affinity == nil {
		newD.Spec.Template.Spec.Affinity = &v1.Affinity{}
	}

	if newD.Spec.Template.Spec.Affinity.NodeAffinity == nil {
		newD.Spec.Template.Spec.Affinity.NodeAffinity = &v1.NodeAffinity{}
	}

	if newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &v1.NodeSelector{}
	}

	if newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms == nil {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []v1.NodeSelectorTerm{}
	}

	selectorTerm := v1.NodeSelectorTerm{}
	selectorTerm.MatchExpressions = append(selectorTerm.MatchExpressions, defaultExpression)
	newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, selectorTerm)

	newData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &apps.DaemonSet{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().DaemonSets(s.Namespace).Patch(context.TODO(), s.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		logrus.Errorf("failed to patch annotation for daemonsets, daemonsets: %v, err: %v", s.Name, err)
		return nil, err
	}

	return result, nil
}

func (c *controller) patchDaemonsetWithoutSpecificNodeAffinity(s *apps.DaemonSet, key, value string) (*apps.DaemonSet, error) {
	if s == nil {
		return nil, fmt.Errorf("unexpected statefulset as nil")
	}

	oldData, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	newD := s.DeepCopy()
	if newD.Spec.Template.Spec.Affinity == nil ||
		newD.Spec.Template.Spec.Affinity.NodeAffinity == nil ||
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms == nil ||
		len(newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {

		return nil, nil
	}

	var newSelectorTerm = []v1.NodeSelectorTerm{}
	for _, st := range newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		var newExpression = []v1.NodeSelectorRequirement{}
		for _, express := range st.MatchExpressions {
			// remove default expression patched by kubefree
			if express.Key != defaultExpression.Key {
				newExpression = append(newExpression, express)
			}
		}

		if len(newExpression) != 0 {
			var newNodeSelectorTerm = st
			newNodeSelectorTerm.MatchExpressions = newExpression
			newSelectorTerm = append(newSelectorTerm, newNodeSelectorTerm)
		}
	}

	if len(newSelectorTerm) != 0 {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = newSelectorTerm
	} else {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	}

	newData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &apps.DaemonSet{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().DaemonSets(s.Namespace).Patch(context.TODO(), s.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		logrus.Errorf("failed to patch annotation for daemonsets, daemonsets: %v, err: %v ", s.Name, err)
		return nil, err
	}

	return result, nil
}

type namespaceController struct {
	controller
}

// target delete-after rules
func (c *namespaceController) syncDeleteAfterRules(namespace *v1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.DeleteAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have delete-after label, do nothing
		return nil
	}

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration && namespace.Status.Phase != v1.NamespaceTerminating {
		nl := logrus.WithField("namespace", namespace.Name).
			WithField("lastActivityTime", lastActivity).
			WithField("delete-after", v)
		if !c.DryRun {
			if err != c.clientset.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{}) {
				nl.WithError(err).Errorln("error delete namespace")
				return err
			}

			nl.Info("delete inactivity namespace success")
		}
	}
	return nil
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

	lg := logrus.WithField("namespace", namespace.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("sleep-after", v)

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
						lg.WithError(err).Error("Error delete namespace")
						return err
					}
					lg.Infoln("delete inactivity namespace successfully")
				}
			}
		default:
			/*
				Enter into sleep mode
				step1: update execution state with sleeping
				step2: sleep namespace
				step3: update execution state with sleep
			*/
			lg.Info("try to sleep inactivity namespace")
			if _, err := c.setKubefreeExecutionState(namespace, SLEEPING); err != nil {
				lg.WithError(err).Errorln("failed to SetKubefreeExecutionState")
				return err
			}
			if !c.DryRun {
				if err := c.Sleep(namespace); err != nil {
					lg.WithError(err).Errorln("failed to sleep namespace")
					return err
				}
			}
			if _, err := c.setKubefreeExecutionState(namespace, SLEEP); err != nil {
				lg.WithError(err).Errorln("failed to SetKubefreeExecutionState")
				return err
			}
			lg.Infoln("sleep inactivity namespace successfully")
		}
	} else {
		switch state {
		case SLEEPING, SLEEP:
			lg.Infof("try to wake up namespace %s", namespace.Name)
			if !c.DryRun {
				if err := c.WakeUp(namespace); err != nil {
					lg.WithError(err).Errorln("failed to wake up namespace")
					return err
				}
			}

			if _, err := c.setKubefreeExecutionState(namespace, NORMAL); err != nil {
				lg.WithError(err).Errorln("failed to SetKubefreeExecutionState")
			}
			lg.Infoln("wake up namespace successfully")
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
		logrus.Errorf("failed to patch annotation for namespace, namespace: %v , err: %v", namespace.Name, err)
		return nil, err
	}
	return result, nil
}
