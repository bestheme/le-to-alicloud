//go:build e2e

/*
Copyright 2026 Hangzhou Yunqi Intelligence Technology Co., Ltd.

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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"git.dev.bestheme.ac.cn/infra/le-to-alicloud/test/utils"
)

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	// certManagerInstalledByUs 只有在本套件真的执行完 InstallCertManager 之后才为 true，
	// AfterSuite 据此决定要不要卸载。这里刻意用「装过才拆」的正向标志，而不是脚手架原来
	// 那个「没检测到就拆」的反向标志：反向标志在 BeforeSuite 提前失败时仍然是零值 false，
	// 而 Ginkgo 照样会跑 AfterSuite，于是卸载会打到一个本套件根本没动过的集群上。
	certManagerInstalledByUs = false

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/le-to-alicloud:v0.0.1"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting le-to-alicloud integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// 集群身份闸门，必须放在构建/加载镜像之前。这套 e2e 会在集群上安装、并在 AfterSuite 里
	// 卸载 cert-manager；只要当前 kubeconfig 指的不是一次性 Kind 集群，那次卸载就会打到真
	// 集群上。2026-09-05 与 2026-09-06 各发生过一次：cert-manager 的 namespace 与 6 个 CRD
	// 被从生产 OpenShift 上删掉，且不会自动装回。确实要在非 Kind 集群上跑时，显式设
	// E2E_ALLOW_NON_KIND=1 表示自己清楚会发生什么。
	By("verifying that the current kubeconfig context points at a Kind cluster")
	if os.Getenv("E2E_ALLOW_NON_KIND") != "1" {
		output, err := utils.Run(exec.Command("kubectl", "config", "current-context"))
		Expect(err).NotTo(HaveOccurred(), "Failed to read the current kubeconfig context")
		currentContext := strings.TrimSpace(output)
		Expect(currentContext).To(HavePrefix("kind-"), fmt.Sprintf(
			"refusing to run the e2e suite against context %q: it installs and uninstalls "+
				"cert-manager cluster-wide. Set E2E_ALLOW_NON_KIND=1 to override.", currentContext))
	}

	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// TODO(user): If you want to change the e2e test vendor from Kind, ensure the image is
	// built and available before running the tests. Also, remove the following block.
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with CertManager already installed,
	// we check for its presence before execution.
	// Setup CertManager before the suite if not skipped and if not already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		alreadyInstalled, err := utils.IsCertManagerCRDsInstalled()
		Expect(err).NotTo(HaveOccurred(), "Failed to check whether CertManager CRDs are installed")
		if !alreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
			certManagerInstalledByUs = true
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	// 只拆本套件自己装的那一份。卸载失败要显式断言出来：半拆的 cert-manager 会让后续
	// 运行以莫名其妙的方式失败，收尾的失败不能只留一行 warning。
	if certManagerInstalledByUs {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		Expect(utils.UninstallCertManager()).To(Succeed(), "Failed to uninstall CertManager")
	}
})
