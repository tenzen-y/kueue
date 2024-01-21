/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"sigs.k8s.io/kueue/pkg/manager"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	zaplog "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	autoscaling "k8s.io/autoscaler/cluster-autoscaler/provisioningrequest/apis/autoscaling.x-k8s.io/v1beta1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta1"
	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/config"
	"sigs.k8s.io/kueue/pkg/constants"
	"sigs.k8s.io/kueue/pkg/controller/admissionchecks/multikueue"
	"sigs.k8s.io/kueue/pkg/controller/admissionchecks/provisioning"
	"sigs.k8s.io/kueue/pkg/controller/core"
	"sigs.k8s.io/kueue/pkg/controller/core/indexer"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/debugger"
	"sigs.k8s.io/kueue/pkg/features"
	"sigs.k8s.io/kueue/pkg/queue"
	"sigs.k8s.io/kueue/pkg/scheduler"
	"sigs.k8s.io/kueue/pkg/util/cert"
	"sigs.k8s.io/kueue/pkg/util/kubeversion"
	"sigs.k8s.io/kueue/pkg/visibility"
	"sigs.k8s.io/kueue/pkg/webhooks"

	// Ensure linking of the job controllers.
	_ "sigs.k8s.io/kueue/pkg/controller/jobs"
	// +kubebuilder:scaffold:imports
)

var (
	scheme                       = runtime.NewScheme()
	setupLog                     = ctrl.Log.WithName("setup")
	errPodIntegration            = errors.New("pod integration only supported in Kubernetes 1.27 or newer")
	errServerVersionFetcherIsNil = errors.New("serverVersionFetcher is nil")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(schedulingv1.AddToScheme(scheme))

	utilruntime.Must(kueue.AddToScheme(scheme))
	utilruntime.Must(kueuealpha.AddToScheme(scheme))
	utilruntime.Must(configapi.AddToScheme(scheme))
	utilruntime.Must(autoscaling.AddToScheme(scheme))
	// Add any additional framework integration types.
	utilruntime.Must(
		jobframework.ForEachIntegration(func(_ string, cb jobframework.IntegrationCallbacks) error {
			if cb.AddToScheme != nil {
				return cb.AddToScheme(scheme)
			}
			return nil
		}),
	)

	// +kubebuilder:scaffold:scheme
}

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "",
		"The controller will load its initial configuration from this file. "+
			"Omit this flag to use the default configuration values. ")

	var featureGates string
	flag.StringVar(&featureGates, "feature-gates", "", "A set of key=value pairs that describe feature gates for alpha/experimental features.")

	opts := zap.Options{
		TimeEncoder: zapcore.RFC3339NanoTimeEncoder,
		ZapOpts:     []zaplog.Option{zaplog.AddCaller()},
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	certsReady := make(chan struct{})

	mgr, err := manager.New(scheme, certsReady,
		manager.WithZapOpts(opts),
		manager.WithFeatureGates(featureGates),
		manager.WithConfigFile(configFile),
		manager.WithSetupLog(setupLog))
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	cCache := cache.New(mgr.GetClient(), cache.WithPodsReadyTracking(blockForPodsReady(&mgr.Config)))
	queues := queue.NewManager(mgr.GetClient(), cCache, queue.WithPodsReadyRequeuingTimestamp(podsReadyRequeuingTimestamp(&mgr.Config)))

	ctx := ctrl.SetupSignalHandler()
	if err := setupIndexes(ctx, *mgr); err != nil {
		setupLog.Error(err, "Unable to setup indexes")
		os.Exit(1)
	}
	debugger.NewDumper(cCache, queues).ListenForSignal(ctx)

	// Cert won't be ready until manager starts, so start a goroutine here which
	// will block until the cert is ready before setting up the controllers.
	// Controllers who register after manager starts will start directly.
	go setupControllers(*mgr, cCache, queues, certsReady)

	go func() {
		queues.CleanUpOnContext(ctx)
	}()
	go func() {
		cCache.CleanUpOnContext(ctx)
	}()

	if features.Enabled(features.VisibilityOnDemand) {
		go visibility.CreateAndStartVisibilityServer(queues, ctx)
	}

	setupScheduler(mgr, cCache, queues, &mgr.Config)

	setupLog.Info("Starting manager")
	if err = mgr.Manager.Start(ctx); err != nil {
		setupLog.Error(err, "Could not run manager")
		os.Exit(1)
	}
}

func setupIndexes(ctx context.Context, mgr manager.Manager) error {
	err := indexer.Setup(ctx, mgr.GetFieldIndexer())
	if err != nil {
		return err
	}

	// setup provision admission check controller indexes
	if features.Enabled(features.ProvisioningACC) {
		if !provisioning.ServerSupportsProvisioningRequest(mgr) {
			setupLog.Error(nil, "Provisioning Requests are not supported, skipped admission check controller setup")
		} else if err := provisioning.SetupIndexer(ctx, mgr.GetFieldIndexer()); err != nil {
			setupLog.Error(err, "Could not setup provisioning indexer")
			os.Exit(1)
		}
	}

	if features.Enabled(features.MultiKueue) {
		if err := multikueue.SetupIndexer(ctx, mgr.GetFieldIndexer(), *mgr.Config.Namespace); err != nil {
			setupLog.Error(err, "Could not setup multikueue indexer")
			os.Exit(1)
		}
	}

	opts := []jobframework.Option{
		jobframework.WithEnabledFrameworks(config.EnabledFrameworks(&mgr.Config)),
	}
	return jobframework.SetupIndexes(ctx, mgr, opts...)
}

func setupControllers(mgr manager.Manager, cCache *cache.Cache, queues *queue.Manager, certsReady chan struct{}) {
	// The controllers won't work until the webhooks are operating, and the webhook won't work until the
	// certs are all in place.
	cert.WaitForCertsReady(setupLog, certsReady)

	if failedCtrl, err := core.SetupControllers(mgr, queues, cCache); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", failedCtrl)
		os.Exit(1)
	}

	// setup provision admission check controller
	if features.Enabled(features.ProvisioningACC) && provisioning.ServerSupportsProvisioningRequest(mgr) {
		// An info message is added in setupIndexes if autoscaling is not supported by the cluster
		ctrl, err := provisioning.NewController(mgr.GetClient(), mgr.GetEventRecorderFor("kueue-provisioning-request-controller"))
		if err != nil {
			setupLog.Error(err, "Could not create the provisioning controller")
			os.Exit(1)
		}

		if err := ctrl.SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Could not setup provisioning controller")
			os.Exit(1)
		}
	}

	if features.Enabled(features.MultiKueue) {
		if err := multikueue.SetupControllers(mgr, *mgr.Config.Namespace); err != nil {
			setupLog.Error(err, "Could not setup MultiKueue controller")
			os.Exit(1)
		}
	}

	if failedWebhook, err := webhooks.Setup(mgr); err != nil {
		setupLog.Error(err, "Unable to create webhook", "webhook", failedWebhook)
		os.Exit(1)
	}

	opts := []jobframework.Option{
		jobframework.WithManageJobsWithoutQueueName(mgr.Config.ManageJobsWithoutQueueName),
		jobframework.WithWaitForPodsReady(config.IsWaitForPodsReadyEnable(&mgr.Config)),
		jobframework.WithKubeServerVersion(mgr.ServerVersionFetcher),
		jobframework.WithEnabledFrameworks(config.EnabledFrameworks(&mgr.Config)),
	}
	addPodIntegrationOptsFunc := func(fwkName string, serverVersionFetcher *kubeversion.ServerVersionFetcher, opts ...jobframework.Option) error {
		if fwkName == "pod" {
			if serverVersionFetcher == nil {
				return errServerVersionFetcherIsNil
			}
			v := serverVersionFetcher.GetServerVersion()
			if v.String() == "" || v.LessThan(kubeversion.KubeVersion1_27) {
				return errPodIntegration
			}
			opts = append(
				opts,
				jobframework.WithPodNamespaceSelector(mgr.Config.Integrations.PodOptions.NamespaceSelector),
				jobframework.WithPodSelector(mgr.Config.Integrations.PodOptions.PodSelector),
			)
		}
		return nil
	}
	if err := jobframework.SetupControllers(mgr, setupLog, addPodIntegrationOptsFunc, certsReady, opts...); err != nil {
		setupLog.Error(err, "Unable to create controller or webhook", "kubernetesVersion", mgr.ServerVersionFetcher.GetServerVersion())
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder
}

func setupScheduler(mgr ctrl.Manager, cCache *cache.Cache, queues *queue.Manager, cfg *configapi.Configuration) {
	sched := scheduler.New(
		queues,
		cCache,
		mgr.GetClient(),
		mgr.GetEventRecorderFor(constants.AdmissionName),
		scheduler.WithPodsReadyRequeuingTimestamp(podsReadyRequeuingTimestamp(cfg)),
	)
	if err := mgr.Add(sched); err != nil {
		setupLog.Error(err, "Unable to add scheduler to manager")
		os.Exit(1)
	}
}

func blockForPodsReady(cfg *configapi.Configuration) bool {
	return config.IsWaitForPodsReadyEnable(cfg) && cfg.WaitForPodsReady.BlockAdmission != nil && *cfg.WaitForPodsReady.BlockAdmission
}

func podsReadyRequeuingTimestamp(cfg *configapi.Configuration) configapi.RequeuingTimestamp {
	if cfg.WaitForPodsReady != nil && cfg.WaitForPodsReady.RequeuingTimestamp != nil {
		return *cfg.WaitForPodsReady.RequeuingTimestamp
	}
	return configapi.EvictionTimestamp
}
