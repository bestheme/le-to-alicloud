/*
Copyright 2026.

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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	certsv1alpha1 "git.dev.bestheme.ac.cn/infra/le-to-alicloud/api/v1alpha1"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/internal/controller"
	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/pkg/aliyun"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	// cert-manager 的 Certificate 由本 operator 创建并 watch，必须进 scheme。
	utilruntime.Must(cmapi.AddToScheme(scheme))

	utilruntime.Must(certsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// operatorOptions 是本 operator 自有 flag（spec §9）。
type operatorOptions struct {
	DefaultIssuerName, DefaultIssuerKind, DefaultIssuerGroup     string
	ResyncInterval, DriftCheckInterval, CASProbeInterval         time.Duration
	IssuanceStallThreshold, CloudCallTimeout, CleanupGracePeriod time.Duration
	CleanupFailurePolicy                                         string
	WatchNamespaces                                              []string

	// watchNamespacesRaw 是 --watch-namespaces 的原始逗号分隔值，由 validate 展开。
	watchNamespacesRaw string
}

// registerOperatorFlags 把 flag 注册到给定 FlagSet；main 传 flag.CommandLine，测试传新建的 FlagSet。
// 返回的 options 在 fs.Parse 之后才有值，随后必须调用 validate()。
func registerOperatorFlags(fs *flag.FlagSet) *operatorOptions {
	o := &operatorOptions{}
	fs.StringVar(&o.DefaultIssuerName, "default-issuer-name", "",
		"Name of the Issuer to use when spec.certificateTemplate.issuerRef is not set")
	fs.StringVar(&o.DefaultIssuerKind, "default-issuer-kind", "Issuer", "Kind of the default issuer")
	fs.StringVar(&o.DefaultIssuerGroup, "default-issuer-group", "cert-manager.io", "Group of the default issuer")
	fs.DurationVar(&o.ResyncInterval, "certificate-resync-interval", time.Hour,
		"Periodic resync of AliyunCertificate (Secret drift detection channel)")
	fs.DurationVar(&o.DriftCheckInterval, "drift-check-interval", time.Hour,
		"How often each AliyunCertificateBinding re-reads the certificate on its target to detect and correct drift")
	fs.DurationVar(&o.CASProbeInterval, "cas-probe-interval", 12*time.Hour,
		"How often to verify the current certificate still exists in CAS")
	fs.DurationVar(&o.IssuanceStallThreshold, "issuance-stall-threshold", 6*time.Hour,
		"Issuing=True longer than this marks IssuanceStalled")
	fs.DurationVar(&o.CloudCallTimeout, "cloud-call-timeout", 30*time.Second,
		"Timeout for every Alibaba Cloud API call")
	fs.DurationVar(&o.CleanupGracePeriod, "cleanup-grace-period", 15*time.Minute,
		"How long to retry cloud cleanup in finalizers before applying cleanup-failure-policy")
	fs.StringVar(&o.CleanupFailurePolicy, "cleanup-failure-policy", "Abandon", "Abandon | Block")
	fs.StringVar(&o.watchNamespacesRaw, "watch-namespaces", "", "Comma-separated namespaces to watch; empty = all")
	return o
}

// validate 在 Parse 之后校验并展开派生字段。
func (o *operatorOptions) validate() error {
	if o.CleanupFailurePolicy != controller.CleanupPolicyAbandon &&
		o.CleanupFailurePolicy != controller.CleanupPolicyBlock {
		return fmt.Errorf("--cleanup-failure-policy 必须是 Abandon 或 Block，得到 %q", o.CleanupFailurePolicy)
	}
	o.WatchNamespaces = nil
	for _, n := range strings.Split(o.watchNamespacesRaw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			o.WatchNamespaces = append(o.WatchNamespaces, n)
		}
	}
	return nil
}

// parseOperatorFlags 供测试使用：独立 FlagSet 上注册、解析、校验。
func parseOperatorFlags(args []string) (*operatorOptions, error) {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	o := registerOperatorFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	return o, nil
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	// 值在 flag.Parse() 之后才填充。
	opts := registerOperatorFlags(flag.CommandLine)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	zapOpts := zap.Options{
		Development: true,
	}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if err := opts.validate(); err != nil {
		setupLog.Error(err, "invalid flags")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "certs.bestheme.ac.cn",
		// manager 停止后本进程立刻退出，所以主动让出 lease 是安全的，也让接班的副本不必
		// 空等一整个 LeaseDuration。
		LeaderElectionReleaseOnCancel: true,
		Client: client.Options{Cache: &client.CacheOptions{
			// Secret 不进 cache：零副本，RBAC 不需要 list/watch。
			DisableFor: []client.Object{&corev1.Secret{}},
		}},
	}
	if len(opts.WatchNamespaces) > 0 {
		mgrOpts.Cache.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range opts.WatchNamespaces {
			mgrOpts.Cache.DefaultNamespaces[ns] = cache.Config{}
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := controller.RegisterIndexes(mgr); err != nil {
		setupLog.Error(err, "unable to register indexes")
		os.Exit(1)
	}
	limiters := aliyun.NewLimiters()
	casCache := aliyun.NewClientCache[aliyun.CASClient]()
	certReconciler := &controller.AliyunCertificateReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("aliyuncertificate"),
		// Secret 已 DisableFor，mgr.GetClient() 对它就是直读。
		CASFactory:             controller.NewCASFactory(mgr.GetClient(), casCache, limiters, opts.CloudCallTimeout),
		ResyncInterval:         opts.ResyncInterval,
		CASProbeInterval:       opts.CASProbeInterval,
		IssuanceStallThreshold: opts.IssuanceStallThreshold,
		CleanupGracePeriod:     opts.CleanupGracePeriod,
		CleanupFailurePolicy:   opts.CleanupFailurePolicy,
	}
	certReconciler.SetIssuerDefaults(controller.IssuerDefaults{
		Name:  opts.DefaultIssuerName,
		Kind:  opts.DefaultIssuerKind,
		Group: opts.DefaultIssuerGroup,
	})
	if err := certReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AliyunCertificate")
		os.Exit(1)
	}

	fc3Cache := aliyun.NewClientCache[aliyun.FC3Client]()
	bindingReconciler := &controller.AliyunCertificateBindingReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("aliyuncertificatebinding"),
		// Secret 已 DisableFor，mgr.GetClient() 对它就是直读。
		ProviderFactory:      controller.NewProviderFactory(mgr.GetClient(), fc3Cache, limiters, opts.CloudCallTimeout),
		DriftCheckInterval:   opts.DriftCheckInterval,
		CleanupGracePeriod:   opts.CleanupGracePeriod,
		CleanupFailurePolicy: opts.CleanupFailurePolicy,
	}
	if err := bindingReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AliyunCertificateBinding")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
