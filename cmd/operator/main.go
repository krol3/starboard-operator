package main

import (
	"errors"
	"fmt"

	"github.com/aquasecurity/starboard-operator/pkg/logs"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/aquasecurity/starboard-operator/pkg/aqua"

	"github.com/aquasecurity/starboard-operator/pkg/scanner"
	"github.com/aquasecurity/starboard-operator/pkg/trivy"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/aquasecurity/starboard-operator/pkg/reports"

	"github.com/aquasecurity/starboard-operator/pkg/controllers"
	"github.com/aquasecurity/starboard-operator/pkg/etc"
	starboardv1alpha1 "github.com/aquasecurity/starboard/pkg/apis/aquasecurity/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	// GoReleaser sets three ldflags:
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	versionInfo = etc.VersionInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	}
)

var (
	scheme   = runtime.NewScheme()
	setupLog = logf.Log.WithName("starboard-operator.main")
)

func init() {
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = starboardv1alpha1.AddToScheme(scheme)
}

func main() {
	logf.SetLogger(zap.New())

	if err := run(); err != nil {
		setupLog.Error(err, "Unable to run manager")
	}
}

func run() error {
	setupLog.Info("Starting operator", "version", versionInfo)
	config, err := etc.GetOperatorConfig()
	if err != nil {
		return fmt.Errorf("getting operator config: %w", err)
	}

	// Validate configured namespaces
	operatorNamespace, err := config.GetOperatorNamespace()
	if err != nil {
		return fmt.Errorf("getting operator namespace: %w", err)
	}

	targetNamespaces, err := config.GetTargetNamespaces()
	if err != nil {
		return fmt.Errorf("getting target namespaces: %w", err)
	}

	setupLog.Info("Resolving multitenancy support",
		"operatorNamespace", operatorNamespace,
		"targetNamespaces", targetNamespaces)

	mode, err := etc.ResolveInstallMode(operatorNamespace, targetNamespaces)
	if err != nil {
		return fmt.Errorf("resolving install mode: %w", err)
	}
	setupLog.Info("Resolving install mode", "mode", mode)

	// Set the default manager options.
	options := manager.Options{
		Scheme: scheme,
	}

	if len(targetNamespaces) == 1 && targetNamespaces[0] == operatorNamespace {
		// Add support for OwnNamespace set in STARBOARD_TARGET_NAMESPACES (e.g. ns1).
		setupLog.Info("Constructing single-namespaced cache", "namespace", targetNamespaces[0])
		options.Namespace = targetNamespaces[0]
	} else {
		// Add support for SingleNamespace and MultiNamespace set in STARBOARD_TARGET_NAMESPACES (e.g. ns1,ns2).
		// Note that we may face performance issues when using this with a high number of namespaces.
		// More: https://godoc.org/github.com/kubernetes-sigs/controller-runtime/pkg/cache#MultiNamespacedCacheBuilder
		cachedNamespaces := append(targetNamespaces, operatorNamespace)
		setupLog.Info("Constructing multi-namespaced cache", "namespaces", cachedNamespaces)
		options.Namespace = ""
		options.NewCache = cache.MultiNamespacedCacheBuilder(cachedNamespaces)
	}

	kubernetesConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("getting kube client config: %w", err)
	}

	// The only reason we're using kubernetes.Clientset is that we need it to read Pod logs,
	// which is not supported by the client returned by the ctrl.Manager.
	kubernetesClientset, err := kubernetes.NewForConfig(kubernetesConfig)
	if err != nil {
		return fmt.Errorf("constructing kube client: %w", err)
	}

	mgr, err := ctrl.NewManager(kubernetesConfig, options)
	if err != nil {
		return fmt.Errorf("constructing controllers manager: %w", err)
	}

	scanner, err := getEnabledScanner(config)
	if err != nil {
		return err
	}

	store := reports.NewStore(mgr.GetClient(), scheme)

	if err = (&controllers.PodReconciler{
		Config:  config.Operator,
		Client:  mgr.GetClient(),
		Store:   store,
		Scanner: scanner,
		Log:     ctrl.Log.WithName("controller").WithName("Pod"),
		Scheme:  mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create pod controller: %w", err)
	}

	if err = (&controllers.JobReconciler{
		Config:     config.Operator,
		LogsReader: logs.NewReader(kubernetesClientset),
		Client:     mgr.GetClient(),
		Store:      store,
		Scanner:    scanner,
		Log:        ctrl.Log.WithName("controller").WithName("Job"),
		Scheme:     mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create job controller: %w", err)
	}

	setupLog.Info("Starting controllers manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("starting controllers manager: %w", err)
	}

	return nil
}

func getEnabledScanner(config etc.Config) (scanner.VulnerabilityScanner, error) {
	if config.ScannerTrivy.Enabled && config.ScannerAquaCSP.Enabled {
		return nil, fmt.Errorf("invalid configuration: multiple vulnerability scanners enabled")
	}
	if !config.ScannerTrivy.Enabled && !config.ScannerAquaCSP.Enabled {
		return nil, fmt.Errorf("invalid configuration: none vulnerability scanner enabled")
	}
	if config.ScannerTrivy.Enabled {
		setupLog.Info("Using Trivy as vulnerability scanner", "version", config.ScannerTrivy.Version)
		return trivy.NewScanner(), nil
	}
	if config.ScannerAquaCSP.Enabled {
		setupLog.Info("Using Aqua CSP as vulnerability scanner", "version", config.ScannerAquaCSP.Version)
		return aqua.NewScanner(versionInfo, config.ScannerAquaCSP), nil
	}
	return nil, errors.New("invalid configuration: unhandled vulnerability scanners config")
}
