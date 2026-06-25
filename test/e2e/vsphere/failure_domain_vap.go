package vsphere

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	configclient "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	machineclientv1 "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1"
	machineclientv1beta1 "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1beta1"
	mapiwebhooks "github.com/openshift/machine-api-operator/pkg/webhooks"
	e2eutil "github.com/openshift/machine-api-operator/test/e2e"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	// vapTestMachineSetSuffix is appended to the cluster infra name to form a unique MachineSet name for VAP tests.
	vapTestMachineSetSuffix = "-vap-fd-test"

	// vapTestWaitTimeout is the maximum time to wait for a MachineSet to be deleted.
	vapTestWaitTimeout = 2 * time.Minute
)

// infraWithFDRemoved returns a deep copy of the given Infrastructure with the named failure domain removed
// from spec.platformSpec.vsphere.failureDomains. If no such failure domain exists the original copy is
// returned unchanged.
func infraWithFDRemoved(infra *configv1.Infrastructure, fdName string) *configv1.Infrastructure {
	copy := infra.DeepCopy()
	fds := copy.Spec.PlatformSpec.VSphere.FailureDomains
	filtered := fds[:0]
	for _, fd := range fds {
		if fd.Name != fdName {
			filtered = append(filtered, fd)
		}
	}
	copy.Spec.PlatformSpec.VSphere.FailureDomains = filtered
	return copy
}

// findFDUsedByMachine scans the given machine list and returns the name of the first vSphere
// failure domain that a Machine references via its region/zone labels, plus the matched
// VSpherePlatformFailureDomainSpec. Returns ("", zero-value, false) when no match is found.
func findFDUsedByMachine(
	machines *machinev1beta1.MachineList,
	infra *configv1.Infrastructure,
) (string, configv1.VSpherePlatformFailureDomainSpec, bool) {
	for _, m := range machines.Items {
		region := m.Labels["machine.openshift.io/region"]
		zone := m.Labels["machine.openshift.io/zone"]
		if region == "" || zone == "" {
			continue
		}
		for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
			if fd.Region == region && fd.Zone == zone {
				return fd.Name, fd, true
			}
		}
	}
	return "", configv1.VSpherePlatformFailureDomainSpec{}, false
}

// findFDUsedByCPMS returns the name of the first vSphere failure domain referenced by the
// ControlPlaneMachineSet template, looking at the vsphere failure-domain list.
func findFDUsedByCPMS(cpms *machinev1.ControlPlaneMachineSet, infra *configv1.Infrastructure) (string, bool) {
	template := cpms.Spec.Template.OpenShiftMachineV1Beta1Machine
	if template == nil {
		return "", false
	}
	fds := template.FailureDomains
	if fds == nil || len(fds.VSphere) == 0 {
		return "", false
	}
	cpmsfdName := fds.VSphere[0].Name
	for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
		if fd.Name == cpmsfdName {
			return fd.Name, true
		}
	}
	return "", false
}

// createVAPTestMachineSet creates a zero-replica MachineSet whose template carries
// region/zone labels matching the given failure domain. The MachineSet is named using
// the cluster infra name + vapTestMachineSetSuffix. It clones the provider spec from
// the first existing worker MachineSet.
func createVAPTestMachineSet(
	ctx context.Context,
	cfg *rest.Config,
	mc *machineclientv1beta1.MachineV1beta1Client,
	infra *configv1.Infrastructure,
	fd configv1.VSpherePlatformFailureDomainSpec,
) (*machinev1beta1.MachineSet, error) {
	machineSets, err := e2eutil.GetMachineSets(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not list MachineSets: %w", err)
	}
	if len(machineSets.Items) == 0 {
		return nil, fmt.Errorf("no MachineSets found on the cluster — cannot clone provider spec")
	}

	clonedProvider := machineSets.Items[0].Spec.Template.Spec.ProviderSpec.Value

	msName := infra.Status.InfrastructureName + vapTestMachineSetSuffix

	replicas := int32(0)
	ms := &machinev1beta1.MachineSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MachineSet",
			APIVersion: "machine.openshift.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      msName,
			Namespace: e2eutil.MachineAPINamespace,
			Labels: map[string]string{
				"machine.openshift.io/test": msName,
			},
		},
		Spec: machinev1beta1.MachineSetSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"machine.openshift.io/cluster-api-cluster":    infra.Status.InfrastructureName,
					"machine.openshift.io/cluster-api-machineset": msName,
				},
			},
			Replicas: &replicas,
			Template: machinev1beta1.MachineTemplateSpec{
				ObjectMeta: machinev1beta1.ObjectMeta{
					Labels: map[string]string{
						"machine.openshift.io/cluster-api-machineset":   msName,
						"machine.openshift.io/cluster-api-cluster":      infra.Status.InfrastructureName,
						"machine.openshift.io/cluster-api-machine-role": "worker",
						"machine.openshift.io/cluster-api-machine-type": "worker",
						// The VAP inspects these labels to identify which FD the MachineSet references.
						"machine.openshift.io/region": fd.Region,
						"machine.openshift.io/zone":   fd.Zone,
					},
				},
				Spec: machinev1beta1.MachineSpec{
					ProviderSpec: machinev1beta1.ProviderSpec{
						Value: clonedProvider,
					},
				},
			},
		},
	}

	return mc.MachineSets(e2eutil.MachineAPINamespace).Create(ctx, ms, metav1.CreateOptions{})
}

var _ = Describe(
	"[sig-cluster-lifecycle][OCPFeatureGate:VSphereMultiVCenterDay2][platform:vsphere] vSphere failure domain ValidatingAdmissionPolicies",
	Label("Conformance"), Label("Serial"),
	func() {
		defer GinkgoRecover()

		ctx := context.Background()

		var (
			cfg   *rest.Config
			c     *kubernetes.Clientset
			dc    *dynamic.DynamicClient
			cc    *configclient.ConfigV1Client
			mc    *machineclientv1beta1.MachineV1beta1Client
			mcv1  *machineclientv1.MachineV1Client
			infra *configv1.Infrastructure
			err   error
		)

		BeforeEach(func() {
			cfg, err = e2e.LoadConfig()
			Expect(err).NotTo(HaveOccurred())

			c, err = e2e.LoadClientset()
			Expect(err).NotTo(HaveOccurred())

			dc, err = dynamic.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())

			mc, err = machineclientv1beta1.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())

			mcv1, err = machineclientv1.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())

			cc, err = configclient.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())

			infra, err = cc.Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Ensure Machine API is running on this cluster.
			e2eutil.SkipUnlessMachineAPIOperator(dc, c.CoreV1().Namespaces())

			// All tests in this suite require at least one vSphere failure domain in the infra spec.
			Expect(infra.Spec.PlatformSpec.VSphere).NotTo(BeNil(), "expected vSphere platform spec on Infrastructure/cluster")
			if len(infra.Spec.PlatformSpec.VSphere.FailureDomains) == 0 {
				Skip("skipping — Infrastructure/cluster has no vSphere failure domains configured")
			}
		})

		It("should have three VAPs and three bindings deployed by the operator [apigroup:machine.openshift.io][Suite:openshift/conformance/serial]", func() {
			By("verifying the Machine VAP exists")
			_, err := c.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, mapiwebhooks.VAPMachineFailureDomainName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected ValidatingAdmissionPolicy %q to exist", mapiwebhooks.VAPMachineFailureDomainName)

			By("verifying the CPMS VAP exists")
			_, err = c.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, mapiwebhooks.VAPCPMSFailureDomainName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected ValidatingAdmissionPolicy %q to exist", mapiwebhooks.VAPCPMSFailureDomainName)

			By("verifying the MachineSet VAP exists")
			_, err = c.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, mapiwebhooks.VAPMachineSetFailureDomainName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected ValidatingAdmissionPolicy %q to exist", mapiwebhooks.VAPMachineSetFailureDomainName)

			By("verifying the Machine VAP binding exists")
			_, err = c.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, mapiwebhooks.VAPMachineFailureDomainName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected ValidatingAdmissionPolicyBinding %q to exist", mapiwebhooks.VAPMachineFailureDomainName)

			By("verifying the CPMS VAP binding exists")
			_, err = c.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, mapiwebhooks.VAPCPMSFailureDomainName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected ValidatingAdmissionPolicyBinding %q to exist", mapiwebhooks.VAPCPMSFailureDomainName)

			By("verifying the MachineSet VAP binding exists")
			_, err = c.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, mapiwebhooks.VAPMachineSetFailureDomainName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected ValidatingAdmissionPolicyBinding %q to exist", mapiwebhooks.VAPMachineSetFailureDomainName)
		})

		It("should allow removing an unused failure domain from Infrastructure [apigroup:machine.openshift.io][Suite:openshift/conformance/serial]", func() {
			if len(infra.Spec.PlatformSpec.VSphere.FailureDomains) < 2 {
				Skip("skipping — need at least two failure domains to remove one while keeping another")
			}

			// Find a failure domain not referenced by any Machine or MachineSet.
			machines, err := mc.Machines(e2eutil.MachineAPINamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())

			machineSets, err := e2eutil.GetMachineSets(cfg)
			Expect(err).NotTo(HaveOccurred())

			var unusedFDName string
			for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
				usedByMachine := false
				for _, m := range machines.Items {
					if m.Labels["machine.openshift.io/region"] == fd.Region &&
						m.Labels["machine.openshift.io/zone"] == fd.Zone {
						usedByMachine = true
						break
					}
				}
				if usedByMachine {
					continue
				}
				usedByMS := false
				for _, ms := range machineSets.Items {
					if ms.Spec.Template.ObjectMeta.Labels["machine.openshift.io/region"] == fd.Region &&
						ms.Spec.Template.ObjectMeta.Labels["machine.openshift.io/zone"] == fd.Zone {
						usedByMS = true
						break
					}
				}
				if !usedByMS {
					unusedFDName = fd.Name
					break
				}
			}

			if unusedFDName == "" {
				Skip("skipping — all failure domains are in use by Machines or MachineSets")
			}

			By(fmt.Sprintf("removing unused failure domain %q from Infrastructure", unusedFDName))
			updatedInfra := infraWithFDRemoved(infra, unusedFDName)
			_, err = cc.Infrastructures().Update(ctx, updatedInfra, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected infra update removing unused FD %q to succeed", unusedFDName)

			By("restoring original Infrastructure failure domain list")
			// Re-fetch to get the latest resource version, then restore.
			current, err := cc.Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			current.Spec.PlatformSpec.VSphere.FailureDomains = infra.Spec.PlatformSpec.VSphere.FailureDomains
			_, err = cc.Infrastructures().Update(ctx, current, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected infra restore to succeed")
		})

		It("should block removing a failure domain referenced by a Machine [apigroup:machine.openshift.io][Suite:openshift/conformance/serial]", func() {
			machines, err := mc.Machines(e2eutil.MachineAPINamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())

			fdName, _, found := findFDUsedByMachine(machines, infra)
			if !found {
				Skip("skipping — no existing Machine with region/zone labels matching a known failure domain")
			}

			By(fmt.Sprintf("attempting to remove failure domain %q that is still in use by a Machine", fdName))
			updatedInfra := infraWithFDRemoved(infra, fdName)
			_, err = cc.Infrastructures().Update(ctx, updatedInfra, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "expected infra update removing in-use FD %q to be denied", fdName)
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected a 422/Invalid or 403/Forbidden response, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("Machine"),
				"expected error to mention 'Machine' as the blocking resource")
		})

		It("should block removing a failure domain referenced by a MachineSet [apigroup:machine.openshift.io][Suite:openshift/conformance/serial]", func() {
			machineSets, err := e2eutil.GetMachineSets(cfg)
			Expect(err).NotTo(HaveOccurred())
			if len(machineSets.Items) == 0 {
				Skip("skipping — no MachineSets available to clone for this test")
			}

			// Pick the first failure domain and create a zero-replica MachineSet for it.
			fd := infra.Spec.PlatformSpec.VSphere.FailureDomains[0]

			By(fmt.Sprintf("creating a zero-replica MachineSet referencing failure domain %q (region=%s, zone=%s)", fd.Name, fd.Region, fd.Zone))
			testMS, err := createVAPTestMachineSet(ctx, cfg, mc, infra, fd)
			Expect(err).NotTo(HaveOccurred(), "expected test MachineSet creation to succeed")

			DeferCleanup(func() {
				By("cleaning up test MachineSet")
				if delErr := mc.MachineSets(e2eutil.MachineAPINamespace).Delete(ctx, testMS.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
					e2e.Logf("warning: could not delete test MachineSet %q: %v", testMS.Name, delErr)
				}
			})

			By(fmt.Sprintf("attempting to remove failure domain %q while it is referenced by a MachineSet", fd.Name))
			updatedInfra := infraWithFDRemoved(infra, fd.Name)
			_, err = cc.Infrastructures().Update(ctx, updatedInfra, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "expected infra update removing in-use FD %q to be denied", fd.Name)
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected a 422/Invalid or 403/Forbidden response, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("MachineSet"),
				"expected error to mention 'MachineSet' as the blocking resource")
		})

		It("should block removing a failure domain referenced by a ControlPlaneMachineSet [apigroup:machine.openshift.io][Suite:openshift/conformance/serial]", func() {
			By("fetching the ControlPlaneMachineSet")
			cpms, err := mcv1.ControlPlaneMachineSets(e2eutil.MachineAPINamespace).Get(ctx, "cluster", metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				Skip("skipping — no ControlPlaneMachineSet 'cluster' found on this cluster")
			}
			Expect(err).NotTo(HaveOccurred())

			fdName, found := findFDUsedByCPMS(cpms, infra)
			if !found {
				Skip("skipping — ControlPlaneMachineSet has no vSphere failure domain entries that match Infrastructure")
			}

			By(fmt.Sprintf("attempting to remove failure domain %q that is still referenced by ControlPlaneMachineSet", fdName))
			updatedInfra := infraWithFDRemoved(infra, fdName)
			_, err = cc.Infrastructures().Update(ctx, updatedInfra, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "expected infra update removing in-use FD %q to be denied", fdName)
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected a 422/Invalid or 403/Forbidden response, got: %v", err)
			Expect(err.Error()).To(ContainSubstring("ControlPlaneMachineSet"),
				"expected error to mention 'ControlPlaneMachineSet' as the blocking resource")
		})

		It("should allow removing a failure domain after the referencing MachineSet is deleted [apigroup:machine.openshift.io][Suite:openshift/conformance/serial]", func() {
			machineSets, err := e2eutil.GetMachineSets(cfg)
			Expect(err).NotTo(HaveOccurred())
			if len(machineSets.Items) == 0 {
				Skip("skipping — no MachineSets available to clone for this test")
			}

			if len(infra.Spec.PlatformSpec.VSphere.FailureDomains) < 2 {
				Skip("skipping — need at least two failure domains so we can remove one without breaking the cluster")
			}

			fd := infra.Spec.PlatformSpec.VSphere.FailureDomains[0]

			// Make sure the chosen FD is not referenced by any *existing* Machine (the Machine VAP
			// would also block the removal, which would make this test inconclusive).
			machines, err := mc.Machines(e2eutil.MachineAPINamespace).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, m := range machines.Items {
				if m.Labels["machine.openshift.io/region"] == fd.Region &&
					m.Labels["machine.openshift.io/zone"] == fd.Zone {
					Skip(fmt.Sprintf("skipping — failure domain %q is in use by Machine %q; choose a different FD", fd.Name, m.Name))
				}
			}

			By(fmt.Sprintf("creating a zero-replica MachineSet referencing failure domain %q", fd.Name))
			testMS, err := createVAPTestMachineSet(ctx, cfg, mc, infra, fd)
			Expect(err).NotTo(HaveOccurred())

			// Always attempt cleanup so the MachineSet doesn't leak if the test fails mid-way.
			DeferCleanup(func() {
				if delErr := mc.MachineSets(e2eutil.MachineAPINamespace).Delete(ctx, testMS.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
					e2e.Logf("warning: could not delete test MachineSet %q: %v", testMS.Name, delErr)
				}
				// Restore the infra in case the test succeeded and removed the FD.
				current, getErr := cc.Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
				if getErr != nil {
					e2e.Logf("warning: could not get Infrastructure for restore: %v", getErr)
					return
				}
				alreadyPresent := false
				for _, existingFD := range current.Spec.PlatformSpec.VSphere.FailureDomains {
					if existingFD.Name == fd.Name {
						alreadyPresent = true
						break
					}
				}
				if !alreadyPresent {
					current.Spec.PlatformSpec.VSphere.FailureDomains = infra.Spec.PlatformSpec.VSphere.FailureDomains
					if _, restoreErr := cc.Infrastructures().Update(ctx, current, metav1.UpdateOptions{}); restoreErr != nil {
						e2e.Logf("warning: could not restore Infrastructure failure domains: %v", restoreErr)
					}
				}
			})

			By("verifying that the MachineSet VAP blocks removal of the failure domain")
			updatedInfra := infraWithFDRemoved(infra, fd.Name)
			_, err = cc.Infrastructures().Update(ctx, updatedInfra, metav1.UpdateOptions{})
			Expect(err).To(HaveOccurred(), "expected infra update to be denied while MachineSet references FD %q", fd.Name)
			Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(),
				"expected a 422/Invalid or 403/Forbidden response, got: %v", err)

			By("deleting the test MachineSet")
			Expect(mc.MachineSets(e2eutil.MachineAPINamespace).Delete(ctx, testMS.Name, metav1.DeleteOptions{})).To(Succeed())

			By("waiting for the MachineSet to be fully deleted")
			Eventually(func() bool {
				_, getErr := mc.MachineSets(e2eutil.MachineAPINamespace).Get(ctx, testMS.Name, metav1.GetOptions{})
				return apierrors.IsNotFound(getErr)
			}, vapTestWaitTimeout, 5*time.Second).Should(BeTrue(), "MachineSet %q should be deleted within %s", testMS.Name, vapTestWaitTimeout)

			By(fmt.Sprintf("retrying Infrastructure update to remove failure domain %q — should now succeed", fd.Name))
			// Re-fetch so we have the latest resource version.
			freshInfra, err := cc.Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			freshWithFDRemoved := infraWithFDRemoved(freshInfra, fd.Name)
			_, err = cc.Infrastructures().Update(ctx, freshWithFDRemoved, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "expected infra update to succeed after MachineSet referencing FD %q was deleted", fd.Name)
		})
	},
)
