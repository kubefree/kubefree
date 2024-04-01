package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

func (c *controller) checkDeployment(deployment *appsv1.Deployment) error {
	ac, ok := deployment.Annotations[c.ActivityStatusAnnotation]
	if !ok || ac == "" {
		// activity status not exists, do nothing
		return nil
	}

	activity, err := getActivity(ac)
	if err != nil {
		return err
	}

	logrus.Debugf("pick deployment %s", deployment.Name)

	d := &deploymentController{controller: *c}

	// check delete-after-seconds rules
	if err = d.syncDeleteAfterRules(deployment, *activity); err != nil {
		logrus.WithError(err).Errorln("Error SyncDeleteAfterRules")
		return err
	}

	// check sleep-after rules
	if err = d.syncSleepAfterRules(deployment, *activity); err != nil {
		logrus.WithError(err).Errorln("Error SyncSleepAfterRules")
		return err
	}

	return nil
}

type deploymentController struct {
	controller
}

func (dc *deploymentController) syncDeleteAfterRules(deployment *appsv1.Deployment, lastActivity activity) error {
	v, ok := deployment.Labels[dc.DeleteAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have delete-after label, do nothing
		return nil
	}

	lg := logrus.WithField("namespace", deployment.Namespace).
		WithField("deployment", deployment.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("delete-after", v)

	lg.Debug("checking delete-after rules")

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", dc.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		if !dc.DryRun {
			if err != dc.clientset.AppsV1().Deployments(deployment.Namespace).Delete(context.Background(), deployment.Name, metav1.DeleteOptions{}) {
				lg.WithError(err).Errorln("Error delete deployment")
				return err
			}
			lg.Info("delete deployment successfully")
		}
	}
	return nil
}

func (dc *deploymentController) syncSleepAfterRules(deployment *appsv1.Deployment, lastActivity activity) error {
	v, ok := deployment.Labels[dc.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}

	lg := logrus.WithField("namespace", deployment.Namespace).
		WithField("deployment", deployment.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("sleep-after", v)

	lg.Debug("checking sleep-after rules")

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", dc.SleepAfterSelector, v)
	}

	state := deployment.Labels[dc.ExecutionStateSelector]
	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		switch state {
		case DELETING:
			// do nothing
		case SLEEP, SLEEPING:
			// delete deployment if it's inactivity for 2*thresholdDuration
			if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration*2 {
				if !dc.DryRun {
					if err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Delete(context.Background(), deployment.Name, metav1.DeleteOptions{}); err != nil {
						lg.WithError(err).Error("Error delete deployment")
						return err
					}
					lg.Infoln("delete inactivity deployment successfully")
				}
			}
		default:
			/*
				Enter into sleep mode
				step1: update execution state with sleeping
				step2: sleep namespace
				step3: update execution state with sleep
			*/
			lg.Info("try to sleep inactivity deployment")
			if _, err := dc.setKubefreeExecutionState(deployment, SLEEPING); err != nil {
				lg.WithError(err).Errorln("failed to SetKubefreeExecutionState")
				return err
			}
			if !dc.DryRun {
				if err := dc.sleep(deployment); err != nil {
					lg.WithError(err).Errorln("failed to sleep deployment")
					return err
				}
			}
			if _, err := dc.setKubefreeExecutionState(deployment, SLEEP); err != nil {
				lg.WithError(err).Errorln("failed to SetKubefreeExecutionState")
				return err
			}
			lg.Infoln("sleep inactivity deployment successfully")
		}
	} else {
		switch state {
		case SLEEPING, SLEEP:
			lg.Infof("try to wake up deployment %s", deployment.Name)
			if !dc.DryRun {
				if err := dc.wakeUp(deployment); err != nil {
					lg.WithError(err).Errorln("failed to wake up deployment")
					return err
				}
			}

			if _, err := dc.setKubefreeExecutionState(deployment, NORMAL); err != nil {
				lg.WithError(err).Errorln("failed to SetKubefreeExecutionState")
			}
			lg.Infoln("wake up deployment successfully")
		default:
			// still in activity time scope,so do nothing
		}
	}

	return nil
}

func (dc *deploymentController) setKubefreeExecutionState(deployment *appsv1.Deployment, state string) (*appsv1.Deployment, error) {
	de, err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Get(context.TODO(), deployment.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	oldNsData, err := json.Marshal(de)
	if err != nil {
		return nil, err
	}

	if de.Labels == nil {
		de.Labels = make(map[string]string)
	}

	de.Labels[dc.ExecutionStateSelector] = state
	newNsData, err := json.Marshal(de)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNsData, newNsData, &appsv1.Deployment{})
	if err != nil {
		return nil, err
	}

	result, err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Patch(context.TODO(), deployment.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		logrus.Errorf("failed to patch annotation for deployment, deployment: %v , err: %v", fmt.Sprintf("%s/%s", deployment.Namespace, deployment.Name), err)
		return nil, err
	}
	return result, nil
}

func (dc *deploymentController) sleep(deployment *appsv1.Deployment) error {
	de, err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Get(context.TODO(), deployment.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if _, err := dc.updateAnnotation(de, LegacyReplicasAnnotation, fmt.Sprintf("%d", *de.Spec.Replicas)); err != nil {
		return fmt.Errorf("failed to set annotation for deployment %s, err: %v", de.Name, err)
	}

	_, err = dc.scale(context.TODO(), de.Name, de.Namespace, "0", schema.GroupResource{Group: "apps", Resource: "deployments"})
	if err != nil {
		return fmt.Errorf("failed to update scale for deployment %s/%s, with err %v", de.Namespace, de.Name, err)
	}

	return nil
}

func (dc *deploymentController) wakeUp(deployment *appsv1.Deployment) error {
	de, err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Get(context.TODO(), deployment.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	v, ok := de.Annotations[LegacyReplicasAnnotation]
	if !ok || len(v) == 0 {
		return fmt.Errorf("legacy replicas annotation not found or empty for deployment %s", de.Name)
	}

	vInt, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("failed to parse legacy replicas annotation for deployment %s, value: %v,err: %v", de.Name, v, err)
	}

	if int32(vInt) == *de.Spec.Replicas {
		// no need to wake up
		return nil
	}

	_, err = dc.scale(context.TODO(), de.Name, de.Namespace, v, schema.GroupResource{Group: "apps", Resource: "deployments"})
	if err != nil {
		logrus.WithField("namespace", de.Namespace).WithField("deployment", de.Name).WithField("action", "wakeUp").WithError(err).Error("failed to wake up deployment")
		return err
	}

	// remove legacy replicas annotation after wake up to avoid duplicate wake up
	if _, err := dc.deleteAnnotation(de, LegacyReplicasAnnotation); err != nil {
		return fmt.Errorf("failed to patch deployment %s, err: %v", de.Name, err)
	}

	logrus.WithField("namespace", de.Namespace).WithField("deployment", de.Name).Info("wake up deployment successfully")
	return nil
}

func (dc *deploymentController) updateAnnotation(d *appsv1.Deployment, annotation, value string) (*appsv1.Deployment, error) {
	deployment, err := dc.clientset.AppsV1().Deployments(d.Namespace).Get(context.TODO(), d.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment %s, err: %v", deployment.Name, err)
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

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldDeploymentData, newDeploymentData, &appsv1.Deployment{})
	if err != nil {
		return nil, err
	}

	result, err := dc.clientset.AppsV1().Deployments(d.Namespace).Patch(context.TODO(), d.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (dc *deploymentController) deleteAnnotation(deployment *appsv1.Deployment, annotationKey string) (*appsv1.Deployment, error) {
	deployment, err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Get(context.TODO(), deployment.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment %s, err: %v", deployment.Name, err)
	}

	deploymentCopy := deployment.DeepCopy()
	annotations := deploymentCopy.ObjectMeta.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
	}

	_, exist := annotations[annotationKey]
	if !exist {
		return deployment, nil
	}
	delete(annotations, annotationKey)

	patchData, err := yaml.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annotations,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal patch data, err: %v", err)
	}

	de, err := dc.clientset.AppsV1().Deployments(deployment.Namespace).Patch(context.TODO(), deployment.Name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to patch deployment %s, err: %v", deployment.Name, err)
	}

	return de, nil
}
