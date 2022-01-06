package main

import (
	"flag"
	"fmt"

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
	flag.StringVar(&logLevel, "log-level", logrus.InfoLevel.String(), fmt.Sprintf("Logging level, one of %v", logrus.AllLevels))
	var kubeconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	var dryRun bool
	flag.BoolVar(&dryRun, "dryRun", false, "If set, kubefree controller will not delete or sleep namespace, but still annotate it")
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

	ctl, err := ctl.NewController(clientset)
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

	// wait forever
	select {}
}
