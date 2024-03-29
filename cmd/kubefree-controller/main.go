package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	ctl "github.com/kubefree/pkg/controller"
	"github.com/kubefree/pkg/logrusutil"
)

func main() {
	logrusutil.ComponentInit("kubefree-controller")

	var logLevel string
	var kubeconfig string
	var dryRun bool
	var resyncDuration time.Duration

	flag.StringVar(&logLevel, "log-level", logrus.InfoLevel.String(), fmt.Sprintf("Logging level, one of %v", logrus.AllLevels))
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.BoolVar(&dryRun, "dryRun", false, "If set, kubefree controller will not delete or sleep namespace, but still annotate it")
	flag.DurationVar(&resyncDuration, "resyncDuration", time.Minute, "Resync duration for kubefree controller to list all namespaces")
	flag.Parse()

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Error logrus.ParseLevel")
	}
	logrus.SetLevel(level)

	var config *rest.Config
	if len(kubeconfig) > 0 {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			logrus.WithError(err).Fatal("Error clientcmd.BuildConfigFromFlags")
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			logrus.WithError(err).Fatal("Error logrus.ParseLevel")
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.WithError(err).Fatal("Error kubernetes.NewForConfig")
	}

	ctl, err := ctl.NewController(clientset, resyncDuration)
	if err != nil {
		logrus.WithError(err).Fatal("Error plank.NewController")
	}

	if dryRun {
		logrus.Info("Running in dryRun mode")
		ctl.DryRun = dryRun
	}

	stop := make(chan struct{})
	defer close(stop)

	//TODO: workers should be configurable
	go ctl.Run(2, stop)

	logrus.Infoln("kubefree-controller started")

	// wait forever
	select {}
}
