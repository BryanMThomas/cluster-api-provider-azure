//go:build e2e
// +build e2e

/*
Copyright 2020 The Kubernetes Authors.

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

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	aadpodv1 "github.com/Azure/aad-pod-identity/pkg/apis/aadpodidentity/v1"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/preview/monitor/mgmt/2019-06-01/insights"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/blang/semver"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	"github.com/onsi/ginkgo/reporters"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1beta1"
	expv1 "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1beta1"
	clusterv1exp "sigs.k8s.io/cluster-api/exp/api/v1beta1"
	capi_e2e "sigs.k8s.io/cluster-api/test/e2e"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubesystem  = "kube-system"
	activitylog = "azure-activity-logs"
)

// Test suite flags
var (
	// configPath is the path to the e2e config file.
	configPath string

	// useExistingCluster instructs the test to use the current cluster instead of creating a new one (default discovery rules apply).
	useExistingCluster bool

	// artifactFolder is the folder to store e2e test artifacts.
	artifactFolder string

	// skipCleanup prevents cleanup of test resources e.g. for debug purposes.
	skipCleanup bool
)

// Test suite global vars
var (
	// e2eConfig to be used for this test, read from configPath.
	e2eConfig *clusterctl.E2EConfig

	// clusterctlConfigPath to be used for this test, created by generating a clusterctl local repository
	// with the providers specified in the configPath.
	clusterctlConfigPath string

	// bootstrapClusterProvider manages provisioning of the the bootstrap cluster to be used for the e2e tests.
	// Please note that provisioning will be skipped if e2e.use-existing-cluster is provided.
	bootstrapClusterProvider bootstrap.ClusterProvider

	// bootstrapClusterProxy allows to interact with the bootstrap cluster to be used for the e2e tests.
	bootstrapClusterProxy framework.ClusterProxy

	// kubetestConfigFilePath is the path to the kubetest configuration file
	kubetestConfigFilePath string

	// kubetestRepoListPath
	kubetestRepoListPath string

	// useCIArtifacts specifies whether or not to use the latest build from the main branch of the Kubernetes repository
	useCIArtifacts bool

	// usePRArtifacts specifies whether or not to use the build from a PR of the Kubernetes repository
	usePRArtifacts bool
)

type (
	AzureClusterProxy struct {
		framework.ClusterProxy
	}
	// myEventData is used to be able to Marshal insights.EventData into JSON
	// see https://github.com/Azure/azure-sdk-for-go/issues/8224#issuecomment-614777550
	myEventData insights.EventData
)

func NewAzureClusterProxy(name string, kubeconfigPath string, scheme *runtime.Scheme, options ...framework.Option) *AzureClusterProxy {
	proxy, ok := framework.NewClusterProxy(name, kubeconfigPath, scheme, options...).(framework.ClusterProxy)
	Expect(ok).To(BeTrue(), "framework.NewClusterProxy must implement capi_e2e.ClusterProxy")
	return &AzureClusterProxy{
		ClusterProxy: proxy,
	}
}

func (acp *AzureClusterProxy) CollectWorkloadClusterLogs(ctx context.Context, namespace, name, outputPath string) {
	Byf("Dumping workload cluster %s/%s logs", namespace, name)
	acp.ClusterProxy.CollectWorkloadClusterLogs(ctx, namespace, name, outputPath)

	aboveMachinesPath := strings.Replace(outputPath, "/machines", "", 1)

	Byf("Dumping workload cluster %s/%s kube-system pod logs", namespace, name)
	start := time.Now()
	acp.collectPodLogs(ctx, namespace, name, aboveMachinesPath)
	Byf("Fetching kube-system pod logs took %s", time.Since(start).String())

	Byf("Dumping workload cluster %s/%s Azure activity log", namespace, name)
	start = time.Now()
	acp.collectActivityLogs(ctx, aboveMachinesPath)
	Byf("Fetching activity logs took %s", time.Since(start).String())
}

func (acp *AzureClusterProxy) collectPodLogs(ctx context.Context, namespace string, name string, aboveMachinesPath string) {
	workload := acp.GetWorkloadCluster(ctx, namespace, name)
	pods := &corev1.PodList{}
	Expect(workload.GetClient().List(ctx, pods, client.InNamespace(kubesystem))).To(Succeed())

	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			// Watch each container's logs in a goroutine so we can stream them all concurrently.
			go func(pod corev1.Pod, container corev1.Container) {
				defer GinkgoRecover()

				Byf("Creating log watcher for controller %s/%s, container %s", kubesystem, pod.Name, container.Name)
				logFile := path.Join(aboveMachinesPath, kubesystem, pod.Name, container.Name+".log")
				Expect(os.MkdirAll(filepath.Dir(logFile), 0755)).To(Succeed())

				f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err != nil {
					// Failing to fetch logs should not cause the test to fail
					Byf("Error opening file to write pod logs: %v", err)
					return
				}
				defer f.Close()

				opts := &corev1.PodLogOptions{
					Container: container.Name,
					Follow:    true,
				}

				podLogs, err := workload.GetClientSet().CoreV1().Pods(kubesystem).GetLogs(pod.Name, opts).Stream(ctx)
				if err != nil {
					// Failing to stream logs should not cause the test to fail
					Byf("Error starting logs stream for pod %s/%s, container %s: %v", kubesystem, pod.Name, container.Name, err)
					return
				}
				defer podLogs.Close()

				out := bufio.NewWriter(f)
				defer out.Flush()
				_, err = out.ReadFrom(podLogs)
				if err != nil && err != io.ErrUnexpectedEOF {
					// Failing to stream logs should not cause the test to fail
					Byf("Got error while streaming logs for pod %s/%s, container %s: %v", kubesystem, pod.Name, container.Name, err)
				}
			}(pod, container)
		}
	}
}

func (acp *AzureClusterProxy) collectActivityLogs(ctx context.Context, aboveMachinesPath string) {
	timeoutctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	settings, err := auth.GetSettingsFromEnvironment()
	Expect(err).NotTo(HaveOccurred())
	subscriptionID := settings.GetSubscriptionID()
	authorizer, err := settings.GetAuthorizer()
	Expect(err).NotTo(HaveOccurred())
	activityLogsClient := insights.NewActivityLogsClient(subscriptionID)
	activityLogsClient.Authorizer = authorizer

	groupName := os.Getenv(AzureResourceGroup)
	start := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().UTC().Format(time.RFC3339)

	itr, err := activityLogsClient.ListComplete(timeoutctx, fmt.Sprintf("eventTimestamp ge '%s' and eventTimestamp le '%s' and resourceGroupName eq '%s'", start, end, groupName), "")
	if err != nil {
		// Failing to fetch logs should not cause the test to fail
		Byf("Error fetching activity logs for resource group %s: %v", groupName, err)
		return
	}

	logFile := path.Join(aboveMachinesPath, activitylog, groupName+".log")
	Expect(os.MkdirAll(filepath.Dir(logFile), 0755)).To(Succeed())

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		// Failing to fetch logs should not cause the test to fail
		Byf("Error opening file to write activity logs: %v", err)
		return
	}
	defer f.Close()
	out := bufio.NewWriter(f)
	defer out.Flush()

	for ; itr.NotDone(); err = itr.NextWithContext(timeoutctx) {
		if err != nil {
			Byf("Got error while iterating over activity logs for resource group %s: %v", groupName, err)
			return
		}
		event := itr.Value()
		if to.String(event.Category.Value) != "Policy" {
			b, err := json.MarshalIndent(myEventData(event), "", "    ")
			if err != nil {
				Byf("Got error converting activity logs data to json: %v", err)
			}
			if _, err = out.WriteString(string(b) + "\n"); err != nil {
				Byf("Got error while writing activity logs for resource group %s: %v", groupName, err)
			}
		}
	}
}

func init() {
	flag.StringVar(&configPath, "e2e.config", "", "path to the e2e config file")
	flag.StringVar(&artifactFolder, "e2e.artifacts-folder", "", "folder where e2e test artifact should be stored")
	flag.BoolVar(&useCIArtifacts, "kubetest.use-ci-artifacts", false, "use the latest build from the main branch of the Kubernetes repository. Set KUBERNETES_VERSION environment variable to latest-1.xx to use the build from 1.xx release branch.")
	flag.BoolVar(&usePRArtifacts, "kubetest.use-pr-artifacts", false, "use the build from a PR of the Kubernetes repository")
	flag.BoolVar(&skipCleanup, "e2e.skip-resource-cleanup", false, "if true, the resource cleanup after tests will be skipped")
	flag.BoolVar(&useExistingCluster, "e2e.use-existing-cluster", false, "if true, the test uses the current cluster instead of creating a new one (default discovery rules apply)")
	flag.StringVar(&kubetestConfigFilePath, "kubetest.config-file", "", "path to the kubetest configuration file")
	flag.StringVar(&kubetestRepoListPath, "kubetest.repo-list-path", "", "path to the kubetest repo-list path")
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	junitPath := filepath.Join(artifactFolder, fmt.Sprintf("junit.e2e_suite.%d.xml", config.GinkgoConfig.ParallelNode))
	junitReporter := reporters.NewJUnitReporter(junitPath)
	RunSpecsWithDefaultAndCustomReporters(t, "capz-e2e", []Reporter{junitReporter})
}

// Using a SynchronizedBeforeSuite for controlling how to create resources shared across ParallelNodes (~ginkgo threads).
// The local clusterctl repository & the bootstrap cluster are created once and shared across all the tests.
var _ = SynchronizedBeforeSuite(func() []byte {
	// Before all ParallelNodes.

	Expect(configPath).To(BeAnExistingFile(), "Invalid test suite argument. e2e.config should be an existing file.")
	Expect(os.MkdirAll(artifactFolder, 0755)).To(Succeed(), "Invalid test suite argument. Can't create e2e.artifacts-folder %q", artifactFolder)

	By("Initializing a runtime.Scheme with all the GVK relevant for this test")
	scheme := initScheme()

	Byf("Loading the e2e test configuration from %q", configPath)
	e2eConfig = loadE2EConfig(configPath)

	Byf("Creating a clusterctl local repository into %q", artifactFolder)
	clusterctlConfigPath = createClusterctlLocalRepository(e2eConfig, filepath.Join(artifactFolder, "repository"))

	By("Setting up the bootstrap cluster")
	bootstrapClusterProvider, bootstrapClusterProxy = setupBootstrapCluster(e2eConfig, scheme, useExistingCluster)

	By("Initializing the bootstrap cluster")
	initBootstrapCluster(bootstrapClusterProxy, e2eConfig, clusterctlConfigPath, artifactFolder)

	return []byte(
		strings.Join([]string{
			artifactFolder,
			configPath,
			clusterctlConfigPath,
			bootstrapClusterProxy.GetKubeconfigPath(),
		}, ","),
	)
}, func(data []byte) {
	// Before each ParallelNode.

	parts := strings.Split(string(data), ",")
	Expect(parts).To(HaveLen(4))

	artifactFolder = parts[0]
	configPath = parts[1]
	clusterctlConfigPath = parts[2]
	kubeconfigPath := parts[3]

	e2eConfig = loadE2EConfig(configPath)
	bootstrapClusterProxy = NewAzureClusterProxy("bootstrap", kubeconfigPath, initScheme(),
		framework.WithMachineLogCollector(AzureLogCollector{}))
})

// Using a SynchronizedAfterSuite for controlling how to delete resources shared across ParallelNodes (~ginkgo threads).
// The bootstrap cluster is shared across all the tests, so it should be deleted only after all ParallelNodes completes.
// The local clusterctl repository is preserved like everything else created into the artifact folder.
var _ = SynchronizedAfterSuite(func() {
	// After each ParallelNode.
}, func() {
	// After all ParallelNodes.

	By("Tearing down the management cluster")
	if !skipCleanup {
		tearDown(bootstrapClusterProvider, bootstrapClusterProxy)
	}
})

func initScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	framework.TryAddDefaultSchemes(scheme)
	Expect(infrav1.AddToScheme(scheme)).To(Succeed())
	Expect(expv1.AddToScheme(scheme)).To(Succeed())
	Expect(clusterv1exp.AddToScheme(scheme)).To(Succeed())
	// Add aadpodidentity v1 to the scheme.
	aadPodIdentityGroupVersion := schema.GroupVersion{Group: aadpodv1.GroupName, Version: "v1"}
	scheme.AddKnownTypes(aadPodIdentityGroupVersion,
		&aadpodv1.AzureIdentity{},
		&aadpodv1.AzureIdentityList{},
		&aadpodv1.AzureIdentityBinding{},
		&aadpodv1.AzureIdentityBindingList{},
		&aadpodv1.AzureAssignedIdentity{},
		&aadpodv1.AzureAssignedIdentityList{},
		&aadpodv1.AzurePodIdentityException{},
		&aadpodv1.AzurePodIdentityExceptionList{},
	)
	metav1.AddToGroupVersion(scheme, aadPodIdentityGroupVersion)
	return scheme
}

func loadE2EConfig(configPath string) *clusterctl.E2EConfig {
	config := clusterctl.LoadE2EConfig(context.TODO(), clusterctl.LoadE2EConfigInput{ConfigPath: configPath})
	Expect(config).ToNot(BeNil(), "Failed to load E2E config from %s", configPath)

	resolveKubernetesVersions(config)

	return config
}

func createClusterctlLocalRepository(config *clusterctl.E2EConfig, repositoryFolder string) string {
	createRepositoryInput := clusterctl.CreateRepositoryInput{
		E2EConfig:        config,
		RepositoryFolder: repositoryFolder,
	}

	// Ensuring a CNI file is defined in the config and register a FileTransformation to inject the referenced file as in place of the CNI_RESOURCES envSubst variable.
	Expect(config.Variables).To(HaveKey(capi_e2e.CNIPath), "Missing %s variable in the config", capi_e2e.CNIPath)
	cniPath := config.GetVariable(capi_e2e.CNIPath)
	Expect(cniPath).To(BeAnExistingFile(), "The %s variable should resolve to an existing file", capi_e2e.CNIPath)
	createRepositoryInput.RegisterClusterResourceSetConfigMapTransformation(cniPath, capi_e2e.CNIResources)

	clusterctlConfig := clusterctl.CreateRepository(context.TODO(), createRepositoryInput)
	Expect(clusterctlConfig).To(BeAnExistingFile(), "The clusterctl config file does not exists in the local repository %s", repositoryFolder)
	return clusterctlConfig
}

func setupBootstrapCluster(config *clusterctl.E2EConfig, scheme *runtime.Scheme, useExistingCluster bool) (bootstrap.ClusterProvider, framework.ClusterProxy) {
	var clusterProvider bootstrap.ClusterProvider
	kubeconfigPath := ""
	if !useExistingCluster {
		clusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(context.TODO(), bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
			Name:               config.ManagementClusterName,
			RequiresDockerSock: config.HasDockerProvider(),
			Images:             config.Images,
		})
		Expect(clusterProvider).ToNot(BeNil(), "Failed to create a bootstrap cluster")

		kubeconfigPath = clusterProvider.GetKubeconfigPath()
		Expect(kubeconfigPath).To(BeAnExistingFile(), "Failed to get the kubeconfig file for the bootstrap cluster")
	}

	clusterProxy := NewAzureClusterProxy("bootstrap", kubeconfigPath, scheme)
	Expect(clusterProxy).ToNot(BeNil(), "Failed to get a bootstrap cluster proxy")

	return clusterProvider, clusterProxy
}

func initBootstrapCluster(bootstrapClusterProxy framework.ClusterProxy, config *clusterctl.E2EConfig, clusterctlConfig, artifactFolder string) {
	clusterctl.InitManagementClusterAndWatchControllerLogs(context.TODO(), clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:            bootstrapClusterProxy,
		ClusterctlConfigPath:    clusterctlConfig,
		InfrastructureProviders: config.InfrastructureProviders(),
		LogFolder:               filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName()),
	}, config.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...)
}

func tearDown(bootstrapClusterProvider bootstrap.ClusterProvider, bootstrapClusterProxy framework.ClusterProxy) {
	if bootstrapClusterProxy != nil {
		bootstrapClusterProxy.Dispose(context.TODO())
	}
	if bootstrapClusterProvider != nil {
		bootstrapClusterProvider.Dispose(context.TODO())
	}
}

// resolveKubernetesVersions looks at Kubernetes versions set as variables in the e2e config and sets them to a valid k8s version
// that has an existing capi offer image available. For example, if the version is "stable-1.22", the function will set it to the latest 1.22 version that has a published reference image.
func resolveKubernetesVersions(config *clusterctl.E2EConfig) {
	ubuntuSkus := getImageSkusInOffer(context.TODO(), os.Getenv(AzureLocation), capiImagePublisher, capiOfferName)
	ubuntuVersions := parseImageSkuNames(ubuntuSkus)

	windowsSkus := getImageSkusInOffer(context.TODO(), os.Getenv(AzureLocation), capiImagePublisher, capiWindowsOfferName)
	windowsVersions := parseImageSkuNames(windowsSkus)

	// find the intersection of ubuntu and windows versions available, since we need an image for both.
	var versions semver.Versions
	for k, v := range ubuntuVersions {
		if _, ok := windowsVersions[k]; ok {
			versions = append(versions, v)
		}
	}

	if config.HasVariable(capi_e2e.KubernetesVersion) {
		resolveKubernetesVersion(config, versions, capi_e2e.KubernetesVersion)
	}
	if config.HasVariable(capi_e2e.KubernetesVersionUpgradeFrom) {
		resolveKubernetesVersion(config, versions, capi_e2e.KubernetesVersionUpgradeFrom)
	}
	if config.HasVariable(capi_e2e.KubernetesVersionUpgradeTo) {
		resolveKubernetesVersion(config, versions, capi_e2e.KubernetesVersionUpgradeTo)
	}
}

func resolveKubernetesVersion(config *clusterctl.E2EConfig, versions semver.Versions, varName string) {
	v := getLatestSkuForMinor(config.GetVariable(varName), versions)
	if _, ok := os.LookupEnv(varName); ok {
		Expect(os.Setenv(varName, v)).To(Succeed())
	}
	config.Variables[varName] = v
}

// getImageSkusInOffer returns all skus for an offer that have at least one image.
func getImageSkusInOffer(ctx context.Context, location, publisher, offer string) []string {
	settings, err := auth.GetSettingsFromEnvironment()
	Expect(err).NotTo(HaveOccurred())
	subscriptionID := settings.GetSubscriptionID()
	authorizer, err := settings.GetAuthorizer()
	Expect(err).NotTo(HaveOccurred())
	imagesClient := compute.NewVirtualMachineImagesClient(subscriptionID)
	imagesClient.Authorizer = authorizer

	Byf("Finding image skus for offer %s/%s in %s", publisher, offer, location)

	res, err := imagesClient.ListSkus(ctx, location, publisher, offer)
	Expect(err).NotTo(HaveOccurred())

	var skus []string
	if res.Value != nil {
		skus = make([]string, len(*res.Value))
		for i, sku := range *res.Value {
			// we have to do this to make sure the SKU has existing images
			// see https://github.com/Azure/azure-cli/issues/20115.
			res, err := imagesClient.List(ctx, location, publisher, offer, *sku.Name, "", nil, "")
			Expect(err).NotTo(HaveOccurred())
			if res.Value != nil && len(*res.Value) > 0 {
				skus[i] = *sku.Name
			}
		}
	}
	return skus
}

// parseImageSkuNames parses SKU names in format "k8s-1dot17dot2-os-123" to extract the Kubernetes version.
// it returns a sorted list of all k8s versions found.
func parseImageSkuNames(skus []string) map[string]semver.Version {
	capiSku := regexp.MustCompile(`^k8s-(0|[1-9][0-9]*)dot(0|[1-9][0-9]*)dot(0|[1-9][0-9]*)-[a-z]*.*$`)
	versions := make(map[string]semver.Version, len(skus))
	for _, sku := range skus {
		match := capiSku.FindStringSubmatch(sku)
		if len(match) != 0 {
			stringVer := fmt.Sprintf("%s.%s.%s", match[1], match[2], match[3])
			versions[stringVer] = semver.MustParse(stringVer)
		}
	}

	return versions
}

// getLatestSkuForMinor gets the latest available patch version in the provided list of sku versions that corresponds to the provided k8s version.
func getLatestSkuForMinor(version string, skus semver.Versions) string {
	isStable, match := validateStableReleaseString(version)
	if isStable {
		// if the version is in the format "stable-1.21", we find the latest 1.21.x version.
		major, err := strconv.ParseUint(match[1], 10, 64)
		Expect(err).NotTo(HaveOccurred())
		minor, err := strconv.ParseUint(match[2], 10, 64)
		Expect(err).NotTo(HaveOccurred())
		semver.Sort(skus)
		for i := len(skus) - 1; i >= 0; i-- {
			if skus[i].Major == major && skus[i].Minor == minor {
				version = "v" + skus[i].String()
				break
			}
		}
	} else if v, err := semver.ParseTolerant(version); err == nil {
		// if the version is in the format "v1.21.2", we make sure we have an existing image for it.
		Expect(skus).To(ContainElement(v), fmt.Sprintf("Provided Kubernetes version %s does not have a corresponding VM image in the capi offer", version))
	}
	// otherwise, we just return the version as-is. This allows for versions in other formats, such as "latest" or "latest-1.21".
	return version
}

// validateStableReleaseString validates the string format that declares "get be the latest stable release for this <Major>.<Minor>"
// it should be called wherever we process a stable version string expression like "stable-1.22"
func validateStableReleaseString(stableVersion string) (bool, []string) {
	stableReleaseFormat := regexp.MustCompile(`^stable-(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	matches := stableReleaseFormat.FindStringSubmatch(stableVersion)
	return len(matches) > 0, matches
}
