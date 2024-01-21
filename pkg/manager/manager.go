package manager

import (
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta1"
	"sigs.k8s.io/kueue/pkg/config"
	"sigs.k8s.io/kueue/pkg/metrics"
	"sigs.k8s.io/kueue/pkg/util/cert"
	"sigs.k8s.io/kueue/pkg/util/kubeversion"
	"sigs.k8s.io/kueue/pkg/util/useragent"
	"sigs.k8s.io/kueue/pkg/version"
)

type Options struct {
	ZapOpts      zap.Options
	FeatureGates string
	ConfigFile   string
	SetupLog     logr.Logger
}

type Option func(*Options)

func WithZapOpts(zo zap.Options) Option {
	return func(o *Options) {
		o.ZapOpts = zo
	}
}

func WithFeatureGates(f string) Option {
	return func(o *Options) {
		o.FeatureGates = f
	}
}

func WithConfigFile(c string) Option {
	return func(o *Options) {
		o.ConfigFile = c
	}
}

func WithSetupLog(l logr.Logger) Option {
	return func(o *Options) {
		o.SetupLog = l
	}
}

var DefaultOptions = Options{}

type Manager struct {
	ctrl.Manager

	Config               configapi.Configuration
	ServerVersionFetcher *kubeversion.ServerVersionFetcher
}

func New(scheme *runtime.Scheme, certsReady chan struct{}, opts ...Option) (*Manager, error) {
	options := DefaultOptions
	for _, opt := range opts {
		opt(&options)
	}
	setupLog := options.SetupLog

	if err := utilfeature.DefaultMutableFeatureGate.Set(options.FeatureGates); err != nil {
		return nil, fmt.Errorf("unable to set flag gates for known features: %w", err)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options.ZapOpts)))
	setupLog.Info("Initializing", "gitVersion", version.GitVersion, "gitCommit", version.GitCommit)

	ctrlOpts, cfg, err := config.Apply(options.ConfigFile, scheme, setupLog)
	if err != nil {
		return nil, fmt.Errorf("unable to load the configuration: %w", err)
	}

	metrics.Register()

	kubeConfig := setupKubeConfig(cfg, setupLog)
	mgr, err := ctrl.NewManager(kubeConfig, ctrlOpts)

	if err = cert.ManageCerts(mgr, cfg, certsReady); err != nil {
		return nil, fmt.Errorf("unable to set up cert rotation: %w", err)
	}

	if err = setupProbeEndpoints(mgr, setupLog); err != nil {
		return nil, fmt.Errorf("unable to set up probe endpoints: %w", err)
	}
	svFetcher, err := setupServerVersionFetcher(mgr, kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to set up server version fetcher: %w", err)
	}

	return &Manager{
		Manager:              mgr,
		Config:               cfg,
		ServerVersionFetcher: svFetcher,
	}, nil
}

func setupKubeConfig(cfg configapi.Configuration, setupLog logr.Logger) *rest.Config {
	kubeConfig := ctrl.GetConfigOrDie()
	if kubeConfig.UserAgent == "" {
		kubeConfig.UserAgent = useragent.Default()
	}
	kubeConfig.QPS = *cfg.ClientConnection.QPS
	kubeConfig.Burst = int(*cfg.ClientConnection.Burst)
	setupLog.V(2).Info("K8S Client", "qps", kubeConfig.QPS, "burst", kubeConfig.Burst)
	return kubeConfig
}

// setupProbeEndpoints registers the health endpoints.
func setupProbeEndpoints(mgr ctrl.Manager, setupLog logr.Logger) error {
	defer setupLog.Info("Probe endpoints are configured on healthz and readyz")

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}
	return nil
}

func setupServerVersionFetcher(mgr ctrl.Manager, kubeConfig *rest.Config) (*kubeversion.ServerVersionFetcher, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create the discovery client: %w", err)
	}

	serverVersionFetcher := kubeversion.NewServerVersionFetcher(discoveryClient)

	if err = mgr.Add(serverVersionFetcher); err != nil {
		return nil, fmt.Errorf("unable to add server version fetcher to manager: %w", err)
	}

	if err = serverVersionFetcher.FetchServerVersion(); err != nil {
		return nil, fmt.Errorf("failed to fetch kubernetes server version: %w", err)
	}
	return serverVersionFetcher, nil
}
