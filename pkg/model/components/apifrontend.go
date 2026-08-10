/*
Copyright 2026 The Kubernetes Authors.

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

package components

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"

	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/featureflag"
	"k8s.io/kops/pkg/wellknownports"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/loader"

	"github.com/blang/semver/v4"
)

// KubeAPIFrontendOptionsBuilder adds options for the apiFrontend to the model
type KubeAPIFrontendOptionsBuilder struct {
	*OptionsContext
}

var _ loader.ClusterOptionsBuilder = &KubeAPIFrontendOptionsBuilder{}

// BuildOptions is responsible for filling in the default settings for the kube apiFrontend
func (b *KubeAPIFrontendOptionsBuilder) BuildOptions(cluster *kops.Cluster) error {
	clusterSpec := &cluster.Spec
	if clusterSpec.KubeAPIFrontend == nil {
		clusterSpec.KubeAPIFrontend = &kops.KubeAPIServerConfig{}
	}
	c := clusterSpec.KubeAPIFrontend

	if c.APIServerCount == nil {
		count := b.buildAPIFrontendCount(clusterSpec)
		if count == 0 {
			return fmt.Errorf("no instance groups found")
		}
		c.APIServerCount = new(int32(count))
	}

	// @question: should the question every be able to set this?
	if c.StorageBackend == nil {
		// @note: we can use the first version as we enforce both running the same versions.
		// albeit feels a little weird to do this
		sem, err := semver.Parse(strings.TrimPrefix(clusterSpec.EtcdClusters[0].Version, "v"))
		if err != nil {
			return err
		}
		c.StorageBackend = new(fmt.Sprintf("etcd%d", sem.Major))
	}

	if c.KubeletPreferredAddressTypes == nil {
		// We prioritize the internal IP above the hostname
		c.KubeletPreferredAddressTypes = []string{
			string(v1.NodeInternalIP),
			string(v1.NodeHostName),
			string(v1.NodeExternalIP),
		}
	}

	if clusterSpec.Authentication != nil {
		if clusterSpec.Authentication.Kopeio != nil {
			c.AuthenticationTokenWebhookConfigFile = new("/etc/kubernetes/authn.config")
		}
	}

	if clusterSpec.Authorization == nil || clusterSpec.Authorization.IsEmpty() {
		// Do nothing - use the default as defined by the apiFrontend.
		// In practice unreachable: defaulting sets RBAC when authorization is omitted.
	} else if clusterSpec.Authorization.AlwaysAllow != nil {
		clusterSpec.KubeAPIFrontend.AuthorizationMode = new("AlwaysAllow")
	} else if clusterSpec.Authorization.RBAC != nil {
		clusterSpec.KubeAPIFrontend.AuthorizationMode = new("Node,RBAC")
	}

	if err := b.configureAggregation(clusterSpec); err != nil {
		return nil
	}

	image, err := Image("kube-apiserver", clusterSpec, b.AssetBuilder)
	if err != nil {
		return err
	}
	c.Image = image

	if b.controlPlaneKubernetesVersion.IsLT("1.33") {
		c.CloudProvider = "external"
	}

	c.LogLevel = 2
	c.SecurePort = 443

	if clusterSpec.IsIPv6Only() {
		c.BindAddress = "::"
	} else {
		c.BindAddress = "0.0.0.0"
	}

	c.AllowPrivileged = new(true)
	c.ServiceClusterIPRange = clusterSpec.Networking.ServiceClusterIPRange
	c.EtcdServers = nil
	c.EtcdServersOverrides = nil

	for _, etcdCluster := range clusterSpec.EtcdClusters {
		switch etcdCluster.Name {
		case "main":
			c.EtcdServers = append(c.EtcdServers, fmt.Sprintf("https://127.0.0.1:%d", wellknownports.EtcdMainClientPort))
		case "events":
			// Use HTTP for events etcd when EtcdEventsHTTP feature flag is enabled
			scheme := "https"
			if featureflag.EtcdEventsHTTP.Enabled() {
				scheme = "http"
			}
			c.EtcdServersOverrides = append(c.EtcdServersOverrides, fmt.Sprintf("/events#%s://127.0.0.1:%d", scheme, wellknownports.EtcdEventsClientPort))
		case "leases":
			scheme := "https"
			if featureflag.EtcdEventsHTTP.Enabled() {
				scheme = "http"
			}
			c.EtcdServersOverrides = append(c.EtcdServersOverrides, fmt.Sprintf("coordination.k8s.io/leases#%s://127.0.0.1:%d", scheme, wellknownports.EtcdLeasesClientPort))
		}
	}

	// Based on recommendations from:
	// https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/
	{
		c.EnableAdmissionPlugins = []string{
			"DefaultStorageClass",
			"DefaultTolerationSeconds",
			"LimitRanger",
			"MutatingAdmissionWebhook",
			"NamespaceLifecycle",
			"NodeRestriction",
			"ResourceQuota",
			"RuntimeClass",
			"ServiceAccount",
			"ValidatingAdmissionPolicy",
			"ValidatingAdmissionWebhook",
		}
		c.EnableAdmissionPlugins = append(c.EnableAdmissionPlugins, c.AppendAdmissionPlugins...)
	}

	// We make sure to disable AnonymousAuth
	c.AnonymousAuth = new(false)

	// We query via the kube-apiserver-healthcheck proxy, which listens on port 3990
	c.InsecureBindAddress = ""
	c.InsecurePort = nil

	// If metrics-server is enabled, we want aggregator routing enabled so that requests are load balanced.
	metricsServer := clusterSpec.MetricsServer
	if metricsServer != nil && fi.ValueOf(metricsServer.Enabled) {
		if c.EnableAggregatorRouting == nil {
			c.EnableAggregatorRouting = new(true)
		}
	}

	return nil
}

// buildAPIFrontendCount calculates the count of the api Frontend, essentially the number of node marked as Master role
func (b *KubeAPIFrontendOptionsBuilder) buildAPIFrontendCount(clusterSpec *kops.ClusterSpec) int {
	// The --apiserver-count flag is (generally agreed) to be something we need to get rid of in k8s

	// We should do something like this:

	//count := 0
	//for _, ig := range b.InstanceGroups {
	//	if !ig.IsControlPlane() {
	//		continue
	//	}
	//	size := fi.ValueOf(ig.Spec.MaxSize)
	//	if size == 0 {
	//		size = fi.ValueOf(ig.Spec.MinSize)
	//	}
	//	count += size
	//}

	// But if we do, we end up with a weird dependency on InstanceGroups.  We actually could tolerate
	// that in kops, but we don't really want to.

	// So instead, we assume that the etcd cluster size is the API Server Count.
	// We can re-examine this when we allow separate etcd clusters - at which time hopefully
	// the flag won't exist

	counts := make(map[string]int)
	for _, etcdCluster := range clusterSpec.EtcdClusters {
		counts[etcdCluster.Name] = len(etcdCluster.Members)
	}

	count := counts["main"]

	return count
}

// configureAggregation sets up the aggregation options
func (b *KubeAPIFrontendOptionsBuilder) configureAggregation(clusterSpec *kops.ClusterSpec) error {
	clusterSpec.KubeAPIFrontend.RequestheaderAllowedNames = []string{"aggregator"}
	clusterSpec.KubeAPIFrontend.RequestheaderExtraHeaderPrefixes = []string{"X-Remote-Extra-"}
	clusterSpec.KubeAPIFrontend.RequestheaderGroupHeaders = []string{"X-Remote-Group"}
	clusterSpec.KubeAPIFrontend.RequestheaderUsernameHeaders = []string{"X-Remote-User"}

	return nil
}
