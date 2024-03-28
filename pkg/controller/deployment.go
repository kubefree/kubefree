package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	nl := logrus.WithField("namespace", deployment.Namespace).
		WithField("deployment", deployment.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("delete-after", v)

	nl.Debug("checking delete-after rules")

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", dc.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		if !dc.DryRun {
			if err != dc.clientset.AppsV1().Deployments(deployment.Namespace).Delete(context.Background(), deployment.Name, metav1.DeleteOptions{}) {
				nl.Error("Error delete namespace")
				return err
			}
			nl.Info("delete deployment successfully")
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

	nl := logrus.WithField("namespace", deployment.Namespace).
		WithField("deployment", deployment.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("sleep-after", v)

	nl.Debug("checking sleep-after rules")

	_, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", dc.SleepAfterSelector, v)
	}

	// TODO(CarlJi): add sleep logic

	return nil
}
