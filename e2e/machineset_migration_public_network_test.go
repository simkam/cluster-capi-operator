// Copyright 2026 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"fmt"

	"github.com/aws/aws-sdk-go/service/ec2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	mapiv1beta1 "github.com/openshift/api/machine/v1beta1"
	mapiframework "github.com/openshift/cluster-api-actuator-pkg/pkg/framework"
	capiframework "github.com/openshift/cluster-capi-operator/e2e/framework"
	awsv1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest/komega"
)

var _ = Describe("[sig-cluster-lifecycle][OCPFeatureGate:MachineAPIMigration] MachineSet Migration Public Network",
	Ordered,
	Label("platform:aws"),
	func() {
		BeforeAll(func() {
			if platform != configv1.AWSPlatformType {
				Skip(fmt.Sprintf("Skipping tests on %s, this is only supported on AWS", platform))
			}

			if !capiframework.IsFeatureGateEnabled(ctx, cl, features.FeatureGateMachineAPIMigration) {
				Skip("Skipping, this feature is only supported on MachineAPIMigration enabled clusters")
			}
		})

		Describe("with publicIP and subnet filters migrated from MAPI to CAPI", Ordered, func() {
			var msName string
			var mapiMachineSet *mapiv1beta1.MachineSet
			var capiMachineSet *clusterv1.MachineSet
			var awsMachineTemplate *awsv1.AWSMachineTemplate
			var awsClient *ec2.EC2
			var publicSubnetID, publicSubnetAZ string

			BeforeAll(func() {
				awsClient = createAWSClient(infra.Status.PlatformStatus.AWS.Region)

				publicSubnetID, publicSubnetAZ = getPublicSubnetForCluster(awsClient, clusterName)
				if publicSubnetID == "" {
					Skip("No public subnet found in the cluster VPC, skipping public IP migration test")
				}

				msName = generateName("ms-pub-ip-")
				mapiMachineSet = createMAPIMachineSetWithPublicIP(ctx, cl, msName, publicSubnetAZ)
				capiMachineSet, awsMachineTemplate = waitForMAPIMachineSetMirrors(msName)

				DeferCleanup(func() {
					By("Cleaning up public network MachineSet migration test resources")
					cleanupMachineSetTestResources(ctx, cl,
						[]*clusterv1.MachineSet{capiMachineSet},
						[]*awsv1.AWSMachineTemplate{awsMachineTemplate},
						[]*mapiv1beta1.MachineSet{mapiMachineSet},
					)
				})
			})

			// https://issues.redhat.com/browse/OCPBUGS-58071
			It("should provision machines under CAPI authority with publicIP", func() {
				By("Verifying the CAPI mirror AWSMachineTemplate has PublicIP set to true")
				Eventually(komega.Object(awsMachineTemplate), capiframework.WaitShort, capiframework.RetryShort).Should(
					HaveField("Spec.Template.Spec.PublicIP", HaveValue(BeTrue())),
					"AWSMachineTemplate should have PublicIP=true",
				)

				By("Checking AWSCluster has the public subnet in spec.network.subnets (informational, OCPBUGS-58071)")
				awsCluster := &awsv1.AWSCluster{}
				if err := cl.Get(ctx, client.ObjectKey{Namespace: capiframework.CAPINamespace, Name: clusterName}, awsCluster); err != nil {
					GinkgoWriter.Printf("WARNING: could not get AWSCluster to check subnets: %v\n", err)
				} else {
					found := false

					for _, s := range awsCluster.Spec.NetworkSpec.Subnets {
						if s.ID == publicSubnetID {
							found = true
							break
						}
					}

					if !found {
						GinkgoWriter.Printf("WARNING: AWSCluster %q does not have public subnet %q in spec.network.subnets (OCPBUGS-58071)\n", clusterName, publicSubnetID)
					}
				}

				By("Switching MachineSet authority to ClusterAPI")
				switchMachineSetTemplateAuthoritativeAPI(mapiMachineSet, mapiv1beta1.MachineAuthorityClusterAPI)
				switchMachineSetAuthoritativeAPI(mapiMachineSet, mapiv1beta1.MachineAuthorityClusterAPI)
				verifyMachineSetPausedCondition(mapiMachineSet, mapiv1beta1.MachineAuthorityClusterAPI)
				verifyMachineSetPausedCondition(capiMachineSet, mapiv1beta1.MachineAuthorityClusterAPI)
				verifyMAPIMachineSetSynchronizedCondition(mapiMachineSet, mapiv1beta1.MachineAuthorityClusterAPI)

				By("Scaling up CAPI MachineSet to 1 replica")
				capiframework.ScaleCAPIMachineSet(msName, 1, capiframework.CAPINamespace)

				By("Verifying new CAPI Machine reaches Running phase with a ready node")
				capiframework.WaitForMachineSet(ctx, cl, msName, capiframework.CAPINamespace, capiframework.WaitLong)

				By("Verifying replica counts on both sides")
				verifyMachinesetReplicas(capiMachineSet, 1)
				verifyMachinesetReplicas(mapiMachineSet, 1)

				By("Verifying CAPI Machine is running and unpaused")
				capiMachine := capiframework.GetNewestMachineFromMachineSet(capiMachineSet)
				verifyMachineRunning(cl, capiMachine)
				verifyMachinePausedCondition(capiMachine, mapiv1beta1.MachineAuthorityClusterAPI)

				By("Verifying there is a paused MAPI Machine mirror")
				mapiMachine, err := mapiframework.GetLatestMachineFromMachineSet(ctx, cl, mapiMachineSet)
				Expect(err).ToNot(HaveOccurred(), "failed to get MAPI Machines from MachineSet")
				verifyMachineAuthoritative(mapiMachine, mapiv1beta1.MachineAuthorityClusterAPI)
				verifyMachinePausedCondition(mapiMachine, mapiv1beta1.MachineAuthorityClusterAPI)

				By("Deleting MAPI MachineSet and verifying mirrors are removed")
				Expect(mapiframework.DeleteMachineSets(cl, mapiMachineSet)).To(Succeed(), "Should be able to delete test MachineSet")
				capiframework.WaitForMachineSetsDeleted(capiMachineSet)
				mapiframework.WaitForMachineSetsDeleted(ctx, cl, mapiMachineSet)
				verifyResourceRemoved(awsMachineTemplate)
			})
		})
	})
