/*
Copyright 2026 Trustle, Inc..

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
	"crypto/tls"
	"flag"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	dbosv1alpha1 "github.com/trustle/dbos-version-controller/api/v1alpha1"
	"github.com/trustle/dbos-version-controller/internal/config"
	"github.com/trustle/dbos-version-controller/internal/controller"
	"github.com/trustle/dbos-version-controller/internal/db"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(dbosv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	// Every setting also reads a DVC_ environment variable, which wins over the flag.
	settings := config.Default()
	flag.StringVar(&settings.Namespace, "namespace", settings.Namespace,
		"Namespace holding the WorkerVersionSets and the worker objects (DVC_NAMESPACE).")
	flag.StringVar(&settings.DatabaseURL, "database-url", settings.DatabaseURL,
		"URL of the DBOS system database (DVC_DATABASE_URL).")
	flag.DurationVar(&settings.Interval, "interval", settings.Interval,
		"Time between reconciles of one set (DVC_INTERVAL_SECONDS).")
	flag.DurationVar(&settings.Grace, "grace", settings.Grace,
		"Default time a drained version stays idle before deletion (DVC_GRACE_SECONDS).")
	flag.BoolVar(&settings.DryRun, "dry-run", settings.DryRun,
		"Log every write as \"Would ...\" and change nothing (DVC_DRY_RUN).")
	flag.StringVar(&settings.ManagedBy, "managed-by", settings.ManagedBy,
		"Value of the app.kubernetes.io/managed-by label (DVC_MANAGED_BY).")
	flag.StringVar(&settings.VersionLabel, "version-label", settings.VersionLabel,
		"Label carrying the application version (DVC_VERSION_LABEL).")
	flag.StringVar(&settings.LogLevel, "log-level", settings.LogLevel,
		"Level of the root logger (DVC_LOG_LEVEL).")

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	settings, err := config.FromEnv(settings)
	if err != nil {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
		setupLog.Error(err, "Unusable controller settings")
		os.Exit(1)
	}
	opts.Level = logLevel(settings.LogLevel)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider protects the metrics endpoint with authn/authz.
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName,
			"metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "388ebe24.trustle.com",
		// The controller reads and writes one namespace only.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{settings.Namespace: {}},
		},
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.WorkerVersionSetReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Config:      settings,
		Outstanding: db.NewOutstandingCounter(settings.DatabaseURL, controller.OutstandingSQL),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workerversionset")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager",
		"namespace", settings.Namespace,
		"interval", settings.Interval,
		"grace", settings.Grace,
		"dryRun", settings.DryRun,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// logLevel maps DVC_LOG_LEVEL onto a zap level, keeping the Python names working.
func logLevel(name string) zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return zapcore.DebugLevel
	case "warning", "warn":
		return zapcore.WarnLevel
	case "error", "critical", "fatal":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}
