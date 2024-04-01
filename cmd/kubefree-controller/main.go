package main

import (
	"flag"
	"time"

	"github.com/qiniu/x/log"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	ctl "github.com/kubefree/pkg/controller"
)

func main() {
	var logLevel int
	var kubeconfig string
	var dryRun bool
	var resyncDuration time.Duration

	flag.IntVar(&logLevel, "logLevel", 1, "log level")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.BoolVar(&dryRun, "dryRun", false, "If set, kubefree controller will not delete or sleep namespace, but still annotate it")
	flag.DurationVar(&resyncDuration, "resyncDuration", time.Minute, "Resync duration for kubefree controller to list all namespaces")
	flag.Parse()

	log.SetOutputLevel(logLevel)

	var config *rest.Config
	var err error
	if len(kubeconfig) > 0 {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("Error clientcmd.BuildConfigFromFlags: %v", err)
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Fatal("Error rest.InClusterConfig")
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error kubernetes.NewForConfig: %v", err)
	}

	ctl, err := ctl.NewController(clientset, resyncDuration)
	if err != nil {
		log.Fatalf("Error NewController: %v", err)
	}

	if dryRun {
		log.Info("Dry run mode enabled")
		ctl.DryRun = dryRun
	}

	stop := make(chan struct{})
	defer close(stop)

	//TODO: workers should be configurable
	go ctl.Run(2, stop)

	log.Info("kubefree-controller started")

	// wait forever
	select {}
}
