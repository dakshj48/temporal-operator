// Licensed to Alexandre VILAIN under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Alexandre VILAIN licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package config

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/alexandrevilain/controller-tools/pkg/resource"
	"github.com/alexandrevilain/temporal-operator/api/v1beta1"
	"github.com/alexandrevilain/temporal-operator/internal/metadata"
	"github.com/alexandrevilain/temporal-operator/internal/resource/meta"
	"github.com/alexandrevilain/temporal-operator/internal/resource/mtls/certmanager"
	archivalutil "github.com/alexandrevilain/temporal-operator/pkg/temporal/archival"
	"github.com/alexandrevilain/temporal-operator/pkg/temporal/authorization"
	"github.com/alexandrevilain/temporal-operator/pkg/temporal/config"
	"github.com/alexandrevilain/temporal-operator/pkg/temporal/log"
	"github.com/alexandrevilain/temporal-operator/pkg/temporal/persistence"
	"github.com/alexandrevilain/temporal-operator/pkg/version"
	"go.temporal.io/server/common/cluster"

	temporalconfig "go.temporal.io/server/common/config"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/primitives"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var _ resource.Builder = (*ConfigmapBuilder)(nil)

type ConfigmapBuilder struct {
	instance *v1beta1.TemporalCluster
	scheme   *runtime.Scheme
}

func NewConfigmapBuilder(instance *v1beta1.TemporalCluster, scheme *runtime.Scheme) *ConfigmapBuilder {
	return &ConfigmapBuilder{
		instance: instance,
		scheme:   scheme,
	}
}

func (b *ConfigmapBuilder) Build() client.Object {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        b.instance.ChildResourceName(meta.ServiceConfig),
			Namespace:   b.instance.Namespace,
			Labels:      metadata.GetLabels(b.instance, meta.ServiceConfig, b.instance.Spec.Version, b.instance.Labels),
			Annotations: metadata.GetAnnotations(b.instance.Name, b.instance.Annotations),
		},
	}
}

func (b *ConfigmapBuilder) Enabled() bool {
	return true
}

func (b *ConfigmapBuilder) buildDatastoreConfig(store *v1beta1.DatastoreSpec) (*temporalconfig.DataStore, error) {
	cfg := &temporalconfig.DataStore{}
	switch store.GetType() {
	case v1beta1.PostgresSQLDatastore,
		v1beta1.PostgresSQL12Datastore,
		v1beta1.MySQLDatastore,
		v1beta1.MySQL8Datastore:
		cfg.SQL = persistence.NewSQLConfigFromDatastoreSpec(store)
		cfg.SQL.Password = fmt.Sprintf("{{ .Env.%s }}", store.GetPasswordEnvVarName())
	case v1beta1.CassandraDatastore:
		cfg.Cassandra = persistence.NewCassandraConfigFromDatastoreSpec(store)
		cfg.Cassandra.Password = fmt.Sprintf("{{ .Env.%s }}", store.GetPasswordEnvVarName())
	case v1beta1.ElasticsearchDatastore:
		esCfg, err := persistence.NewElasticsearchConfigFromDatastoreSpec(store)
		if err != nil {
			return nil, fmt.Errorf("can't get elasticsearch config: %w", err)
		}
		cfg.Elasticsearch = esCfg
		cfg.Elasticsearch.Password = fmt.Sprintf("{{ .Env.%s }}", store.GetPasswordEnvVarName())
	case v1beta1.UnknownDatastore:
		return nil, errors.New("unknown datastore")
	}
	return cfg, nil
}

func (b *ConfigmapBuilder) buildPersistenceConfig() (*config.Persistence, error) {
	cfg := &config.Persistence{}

	cfg.NumHistoryShards = b.instance.Spec.NumHistoryShards
	cfg.DefaultStore = b.instance.Spec.Persistence.DefaultStore.Name
	cfg.VisibilityStore = b.instance.Spec.Persistence.VisibilityStore.Name
	cfg.DataStores = map[string]temporalconfig.DataStore{}

	// Instroduced in >= 1.21.x
	if b.instance.Spec.Persistence.SecondaryVisibilityStore != nil {
		cfg.SecondaryVisibilityStore = b.instance.Spec.Persistence.SecondaryVisibilityStore.Name
	}

	// This will be removed for clusters >= 1.23.x
	if b.instance.Spec.Persistence.AdvancedVisibilityStore != nil {
		cfg.AdvancedVisibilityStore = b.instance.Spec.Persistence.AdvancedVisibilityStore.Name
	}

	for _, store := range b.instance.Spec.Persistence.GetDatastores() {
		storeConfig, err := b.buildDatastoreConfig(store)
		if err != nil {
			return nil, err
		}

		cfg.DataStores[store.Name] = *storeConfig
	}

	return cfg, nil
}

func (b *ConfigmapBuilder) buildArchivalConfig() (*temporalconfig.Archival, *temporalconfig.ArchivalNamespaceDefaults) {
	cfg := &temporalconfig.Archival{}
	namespaceDefaults := &temporalconfig.ArchivalNamespaceDefaults{}

	if !b.instance.Spec.Archival.IsEnabled() {
		return cfg, namespaceDefaults
	}

	archival := b.instance.Spec.Archival

	cfg.History = temporalconfig.HistoryArchival{}
	cfg.Visibility = temporalconfig.VisibilityArchival{}

	// Configure provider for both history and visibility even if there is no default config for
	// both of them. The user can choose to provide the provider at cluster-level and enable archival per-namespace.
	if archival.Provider != nil {
		cfg.History.Provider = &temporalconfig.HistoryArchiverProvider{
			Filestore: archivalutil.FilestoreArchiverToTemporalFilestoreArchiver(archival.Provider.Filestore),
			Gstorage:  archivalutil.GCSArchiverToTemporalGstorageArchiver(archival.Provider.GCS),
			S3store:   archivalutil.S3ArchiverToTemporalS3Archiver(archival.Provider.S3),
		}

		cfg.Visibility.Provider = &temporalconfig.VisibilityArchiverProvider{
			Filestore: archivalutil.FilestoreArchiverToTemporalFilestoreArchiver(archival.Provider.Filestore),
			Gstorage:  archivalutil.GCSArchiverToTemporalGstorageArchiver(archival.Provider.GCS),
			S3store:   archivalutil.S3ArchiverToTemporalS3Archiver(archival.Provider.S3),
		}
	}

	if archival.History != nil {
		state := temporalconfig.ArchivalDisabled
		if archival.History.Enabled {
			state = temporalconfig.ArchivalEnabled
		}
		if archival.History.Paused {
			state = temporalconfig.ArchivalPaused
		}

		cfg.History.State = state
		cfg.History.EnableRead = archival.History.EnableRead

		namespaceDefaults.History = temporalconfig.HistoryArchivalNamespaceDefaults{
			State: state,
			URI:   archivalutil.URI(archival.Provider, archival.History),
		}
	}

	if archival.Visibility != nil {
		state := temporalconfig.ArchivalDisabled
		if archival.Visibility.Enabled {
			state = temporalconfig.ArchivalEnabled
		}
		if archival.Visibility.Paused {
			state = temporalconfig.ArchivalPaused
		}

		cfg.Visibility.State = state
		cfg.Visibility.EnableRead = archival.Visibility.EnableRead

		namespaceDefaults.Visibility = temporalconfig.VisibilityArchivalNamespaceDefaults{
			State: state,
			URI:   archivalutil.URI(archival.Provider, archival.Visibility),
		}
	}

	return cfg, namespaceDefaults
}

func (b *ConfigmapBuilder) Update(object client.Object) error {
	configMap := object.(*corev1.ConfigMap)

	persistenceConfig, err := b.buildPersistenceConfig()
	if err != nil {
		return fmt.Errorf("can't build persistence config: %w", err)
	}

	archivalConfig, archivalNamespaceDefaults := b.buildArchivalConfig()

	temporalCfg := config.Config{}
	temporalCfg.Global = temporalconfig.Global{
		Membership: temporalconfig.Membership{
			MaxJoinDuration:  30 * time.Second,
			BroadcastAddress: "{{ default .Env.POD_IP \"0.0.0.0\" }}",
		},
		Authorization: authorization.ToTemporalAuthorization(b.instance.Spec.Authorization),
	}

	temporalCfg.Persistence = *persistenceConfig
	temporalCfg.Log = log.NewSQLConfigFromDatastoreSpec(b.instance.Spec.Log)
	temporalCfg.Archival = *archivalConfig
	temporalCfg.NamespaceDefaults = temporalconfig.NamespaceDefaults{
		Archival: *archivalNamespaceDefaults,
	}
	temporalCfg.ClusterMetadata = &cluster.Config{
		EnableGlobalNamespace:    false,
		FailoverVersionIncrement: 10,
		MasterClusterName:        b.instance.Name,
		CurrentClusterName:       b.instance.Name,
		ClusterInformation: map[string]cluster.ClusterInformation{
			b.instance.Name: {
				Enabled:                true,
				InitialFailoverVersion: 1,
				RPCAddress:             "127.0.0.1:7233",
			},
		},
	}
	temporalCfg.Services = map[string]temporalconfig.Service{
		string(primitives.FrontendService): {
			RPC: temporalconfig.RPC{
				GRPCPort:        int(*b.instance.Spec.Services.Frontend.Port),
				MembershipPort:  int(*b.instance.Spec.Services.Frontend.MembershipPort),
				BindOnLocalHost: false,
				BindOnIP:        "0.0.0.0",
			},
		},
		string(primitives.HistoryService): {
			RPC: temporalconfig.RPC{
				GRPCPort:        int(*b.instance.Spec.Services.History.Port),
				MembershipPort:  int(*b.instance.Spec.Services.History.MembershipPort),
				BindOnLocalHost: false,
				BindOnIP:        "0.0.0.0",
			},
		},
		string(primitives.MatchingService): {
			RPC: temporalconfig.RPC{
				GRPCPort:        int(*b.instance.Spec.Services.Matching.Port),
				MembershipPort:  int(*b.instance.Spec.Services.Matching.MembershipPort),
				BindOnLocalHost: false,
				BindOnIP:        "0.0.0.0",
			},
		},
		string(primitives.WorkerService): {
			RPC: temporalconfig.RPC{
				GRPCPort:        int(*b.instance.Spec.Services.Worker.Port),
				MembershipPort:  int(*b.instance.Spec.Services.Worker.MembershipPort),
				BindOnLocalHost: false,
				BindOnIP:        "0.0.0.0",
			},
		},
	}

	if b.instance.Spec.Version.GreaterOrEqual(version.V1_20_0) {
		if b.instance.Spec.Services.InternalFrontend.IsEnabled() {
			temporalCfg.Services[string(primitives.InternalFrontendService)] = temporalconfig.Service{
				RPC: temporalconfig.RPC{
					GRPCPort:        int(*b.instance.Spec.Services.InternalFrontend.Port),
					MembershipPort:  int(*b.instance.Spec.Services.InternalFrontend.MembershipPort),
					HTTPPort:        int(*b.instance.Spec.Services.InternalFrontend.HTTPPort),
					BindOnLocalHost: false,
					BindOnIP:        "0.0.0.0",
				},
			}
		}
	}

	if !b.instance.Spec.Version.GreaterOrEqual(version.V1_18_0) {
		temporalCfg.PublicClient = temporalconfig.PublicClient{
			HostPort: b.instance.GetPublicClientAddress(),
		}
	}

	// Temporal >= 1.22 provides HTTP endpoint for the frontend
	if b.instance.Spec.Version.GreaterOrEqual(version.V1_22_0) &&
		b.instance.Spec.Services.Frontend.HTTPPort != nil {
		frontend, ok := temporalCfg.Services[string(primitives.FrontendService)]
		if ok {
			frontend.RPC.HTTPPort = int(*b.instance.Spec.Services.Frontend.HTTPPort)
			temporalCfg.Services[string(primitives.FrontendService)] = frontend
		}
	}

	if b.instance.Spec.DynamicConfig != nil {
		temporalCfg.DynamicConfigClient = &dynamicconfig.FileBasedClientConfig{
			Filepath:     "/etc/temporal/config/dynamic_config.yaml",
			PollInterval: b.instance.Spec.DynamicConfig.PollInterval.Duration,
		}
	}

	if b.instance.Spec.Metrics.IsEnabled() {
		temporalCfg.Global.Metrics = &metrics.Config{
			ClientConfig: metrics.ClientConfig{
				Tags: map[string]string{"type": "{{ .Env.SERVICES }}"},
			},
		}

		if b.instance.Spec.Metrics.ExcludeTags != nil {
			temporalCfg.Global.Metrics.ClientConfig.ExcludeTags = b.instance.Spec.Metrics.ExcludeTags
		}

		if b.instance.Spec.Metrics.Prefix != nil {
			temporalCfg.Global.Metrics.ClientConfig.Prefix = *b.instance.Spec.Metrics.Prefix
		}

		if b.instance.Spec.Metrics.PerUnitHistogramBoundaries != nil {
			buckets := make(map[string][]float64)
			p := b.instance.Spec.Metrics.PerUnitHistogramBoundaries
			// Convert map[string][]string to map[string][]float64
			for key, value := range p {
				var floatSlice []float64
				for _, str := range value {
					floatVal, err := strconv.ParseFloat(str, 64)
					if err != nil {
						return fmt.Errorf("can't build metrics config: Error converting %s to float: %w", str, err)
					}
					floatSlice = append(floatSlice, floatVal)
				}
				buckets[key] = floatSlice
			}
			temporalCfg.Global.Metrics.ClientConfig.PerUnitHistogramBoundaries = buckets
		}

		if b.instance.Spec.Metrics.Prometheus != nil && b.instance.Spec.Metrics.Prometheus.ListenPort != nil {
			temporalCfg.Global.Metrics.Prometheus = &metrics.PrometheusConfig{
				TimerType:     "histogram",
				ListenAddress: fmt.Sprintf("0.0.0.0:%d", *b.instance.Spec.Metrics.Prometheus.ListenPort),
			}
		}
	}

	if b.instance.MTLSWithCertManagerEnabled() {
		temporalCfg.Global.TLS = temporalconfig.RootTLS{
			RefreshInterval:  b.instance.Spec.MTLS.RefreshInterval.Duration,
			ExpirationChecks: temporalconfig.CertExpirationValidation{},
		}

		internodeMTLS := b.instance.Spec.MTLS.Internode
		if internodeMTLS == nil {
			internodeMTLS = &v1beta1.InternodeMTLSSpec{}
		}

		internodeIntermediateCAFilePath := path.Join(internodeMTLS.GetIntermediateCACertificateMountPath(), certmanager.TLSCA)
		internodeServerCertFilePath := path.Join(internodeMTLS.GetCertificateMountPath(), certmanager.TLSCert)
		internodeServerKeyFilePath := path.Join(internodeMTLS.GetCertificateMountPath(), certmanager.TLSKey)
		internodeClientTLS := temporalconfig.ClientTLS{
			ServerName:              internodeMTLS.ServerName(b.instance),
			DisableHostVerification: false,
			RootCAFiles:             []string{internodeIntermediateCAFilePath},
			ForceTLS:                true,
		}

		if b.instance.Spec.MTLS.InternodeEnabled() {
			temporalCfg.Global.TLS.Internode = temporalconfig.GroupTLS{
				Client: internodeClientTLS,
				Server: temporalconfig.ServerTLS{
					CertFile: internodeServerCertFilePath,
					KeyFile:  internodeServerKeyFilePath,
					ClientCAFiles: []string{
						internodeIntermediateCAFilePath,
					},
					RequireClientAuth: true,
				},
			}

			// If internode mTLs is enabled and internal frontend is enabled,
			// use internode mTLS certificates for the worker TLS.
			if b.instance.Spec.Services.InternalFrontend.IsEnabled() {
				temporalCfg.Global.TLS.SystemWorker = temporalconfig.WorkerTLS{
					Client:   internodeClientTLS,
					CertFile: internodeServerCertFilePath,
					KeyFile:  internodeServerKeyFilePath,
				}
			}
		}

		if b.instance.Spec.MTLS.FrontendEnabled() {
			frontendMTLS := b.instance.Spec.MTLS.Frontend
			frontendIntermediateCAFilePath := path.Join(frontendMTLS.GetIntermediateCACertificateMountPath(), certmanager.TLSCA)

			temporalCfg.Global.TLS.Frontend = temporalconfig.GroupTLS{
				Server: temporalconfig.ServerTLS{
					CertFile:          path.Join(frontendMTLS.GetCertificateMountPath(), certmanager.TLSCert),
					KeyFile:           path.Join(frontendMTLS.GetCertificateMountPath(), certmanager.TLSKey),
					RequireClientAuth: true,
					ClientCAFiles: []string{
						internodeIntermediateCAFilePath,
						frontendIntermediateCAFilePath,
					},
				},
				Client: temporalconfig.ClientTLS{
					ServerName:              frontendMTLS.ServerName(b.instance),
					DisableHostVerification: false,
					RootCAFiles:             []string{frontendIntermediateCAFilePath},
					ForceTLS:                true,
				},
				PerHostOverrides: map[string]temporalconfig.ServerTLS{},
			}

			// If the Frontend mTLS is enabled, and if the internal frontend with internode mTLS is not enabled, the system worker should use the Frontend mTLS.
			if !(b.instance.Spec.MTLS.InternodeEnabled() && b.instance.Spec.Services.InternalFrontend.IsEnabled()) {
				temporalCfg.Global.TLS.SystemWorker = temporalconfig.WorkerTLS{
					CertFile: path.Join(frontendMTLS.GetWorkerCertificateMountPath(), certmanager.TLSCert),
					KeyFile:  path.Join(frontendMTLS.GetWorkerCertificateMountPath(), certmanager.TLSKey),
					Client: temporalconfig.ClientTLS{
						ServerName:              frontendMTLS.ServerName(b.instance),
						DisableHostVerification: false,
						RootCAFiles:             []string{frontendIntermediateCAFilePath},
						ForceTLS:                true,
					},
				}
			}
		}
	}

	result, err := yaml.Marshal(temporalCfg)
	if err != nil {
		return fmt.Errorf("failed marshaling temporal config: %w", err)
	}

	configMap.Data = map[string]string{
		"config_template.yaml": string(result),
	}

	if err := controllerutil.SetControllerReference(b.instance, configMap, b.scheme); err != nil {
		return fmt.Errorf("failed setting controller reference: %w", err)
	}

	return nil
}
