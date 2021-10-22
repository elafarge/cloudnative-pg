/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/utils"
	"github.com/EnterpriseDB/cloud-native-postgresql/tests"
)

// Set of tests in which we check that operator is able to fail over a new
// primary and bring back the replicas when we drain nodes
var _ = Describe("E2E Drain Node", Serial, Label(tests.LabelDisruptive), func() {
	var nodesWithLabels []string
	const level = tests.Lowest

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
		nodes, _ := env.GetNodeList()
		// We label three nodes where we could run the workloads, and ignore
		// the others. The pods of the clusters created in this test run only
		// where the drain label exists.
		for _, node := range nodes.Items {
			if (node.Spec.Unschedulable != true) && (len(node.Spec.Taints) == 0) {
				nodesWithLabels = append(nodesWithLabels, node.Name)
				cmd := fmt.Sprintf("kubectl label node %v drain=drain --overwrite", node.Name)
				_, _, err := tests.Run(cmd)
				Expect(err).ToNot(HaveOccurred())
			}
			if len(nodesWithLabels) == 3 {
				break
			}
		}
		Expect(len(nodesWithLabels)).Should(BeEquivalentTo(3),
			"Not enough nodes are available for this test")
	})

	AfterEach(func() {
		// Uncordon the cordoned nodes and remove the labels we added in the
		// BeforeEach section
		uncordonAllNodes()
		for _, node := range nodesWithLabels {
			cmd := fmt.Sprintf("kubectl label node %v drain- ", node)
			_, _, err := tests.Run(cmd)
			Expect(err).ToNot(HaveOccurred())
		}
		nodesWithLabels = nil
	})

	Context("Maintenance on, reuse pvc on", func() {
		// Initialize empty global namespace variable
		var namespace string
		const sampleFile = fixturesDir + "/drain-node/cluster-drain-node.yaml"
		const clusterName = "cluster-drain-node"

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() {
				env.DumpClusterEnv(namespace, clusterName,
					"out/"+CurrentSpecReport().LeafNodeText+".log")
			}
		})

		AfterEach(func() {
			err := env.DeleteNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
		})

		// We cordon one node, so pods will run on one or two nodes. This
		// is only to create a harder situation for the operator.
		// We then drain the node containing the primary and expect the pod(s)
		// to be back only when its PVC is available. On GKE with the default
		// storage class and on AKS with Rook this happens immediately. When
		// the storage is bound to the node, we have to uncordon the node
		// first. We uncordon it in all cases and check for the UIDs of the
		// PVC(s).

		It("can drain the primary pod's node with 3 pods on 2 nodes", func() {
			namespace = "drain-node-e2e-pvc-on-two-nodes"
			By("leaving only two nodes uncordoned", func() {
				// mark a node unschedulable so the pods will be distributed only on two nodes
				for _, cordonNode := range nodesWithLabels[:len(nodesWithLabels)-2] {
					cmd := fmt.Sprintf("kubectl cordon %v", cordonNode)
					_, _, err := tests.Run(cmd)
					Expect(err).ToNot(HaveOccurred())
				}
			})

			// Create a cluster in a namespace we'll delete after the test
			err := env.CreateNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			By("waiting for the jobs to be removed", func() {
				// Wait for jobs to be removed
				timeout := 180
				Eventually(func() (int, error) {
					podList, err := env.GetPodList(namespace)
					return len(podList.Items), err
				}, timeout).Should(BeEquivalentTo(3))
			})

			// Load test data
			oldPrimary := clusterName + "-1"
			AssertCreateTestData(namespace, clusterName, "test")

			// We create a mapping between the pod names and the UIDs of
			// their volumes. We do not expect the UIDs to change.
			// We take advantage of the fact that related PVCs and Pods have
			// the same name.
			podList, err := env.GetClusterPodList(namespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			pvcUIDMap := make(map[string]types.UID)
			for _, pod := range podList.Items {
				pvcNamespacedName := types.NamespacedName{
					Namespace: namespace,
					Name:      pod.Name,
				}
				pvc := corev1.PersistentVolumeClaim{}
				err = env.Client.Get(env.Ctx, pvcNamespacedName, &pvc)
				Expect(err).ToNot(HaveOccurred())
				pvcUIDMap[pod.Name] = pvc.GetUID()
			}

			// Drain the node containing the primary pod and store the list of running pods
			podsOnPrimaryNode := drainPrimaryNode(namespace, clusterName)

			By("verifying failover after drain", func() {
				timeout := 180
				// Expect a failover to have happened
				Eventually(func() (string, error) {
					pod, err := env.GetClusterPrimary(namespace, clusterName)
					return pod.Name, err
				}, timeout).ShouldNot(BeEquivalentTo(oldPrimary))
			})

			By("uncordon nodes and check new pods use old pvcs", func() {
				uncordonAllNodes()
				// Ensure evicted pods have restarted and are running.
				// one of them could have become the new primary.
				timeout := 300
				for _, podName := range podsOnPrimaryNode {
					namespacedName := types.NamespacedName{
						Namespace: namespace,
						Name:      podName,
					}
					Eventually(func() (bool, error) {
						pod := corev1.Pod{}
						err := env.Client.Get(env.Ctx, namespacedName, &pod)
						return utils.IsPodActive(pod) && utils.IsPodReady(pod), err
					}, timeout).Should(BeTrue())

					pod := corev1.Pod{}
					err = env.Client.Get(env.Ctx, namespacedName, &pod)
					// Check that the PVC UID hasn't changed
					pvc := corev1.PersistentVolumeClaim{}
					err = env.Client.Get(env.Ctx, namespacedName, &pvc)
					Expect(pvc.GetUID(), err).To(BeEquivalentTo(pvcUIDMap[podName]))
				}
			})

			// Expect the (previously created) test data to be available
			primary, err := env.GetClusterPrimary(namespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			AssertDataExpectedCount(namespace, primary.GetName(), "test", 2)
			assertClusterStandbysAreStreaming(namespace, clusterName)
		})

		// Scenario: all the pods of a cluster are on a single node and another schedulable node exists.
		// We perform the drain the node hosting the primary.
		// If PVCs can be moved: all the replicas will be killed and rescheduled to a different node,
		// then a switchover will be triggered, and the old primary will be killed and moved too.
		// The drain will succeed.
		// We have skipped this scenario on the Local executors, Openshift, EKS, RKE
		// because here PVCs can not be moved, so this all replicas should be killed and can not be rescheduled on a
		// new node as there are none, the primary node can not be killed, therefore the drain fails.

		When("the cluster allows moving PVCs between nodes", func() {
			BeforeEach(func() {
				// AKS using rook and the standard GKE StorageClass allow moving PVCs between nodes
				isAKS, err := env.IsAKS()
				Expect(err).ToNot(HaveOccurred())
				isGKE, err := env.IsGKE()
				Expect(err).ToNot(HaveOccurred())
				if !(isAKS || isGKE) {
					Skip("This test case is only applicable on clusters where PVC can be moved")
				}
			})
			It("can drain the primary pod's node with 3 pods on 1 nodes", func() {
				namespace = "drain-node-e2e-pvc-on-one-nodes"
				var cordonNodes []string
				By("leaving only one node uncordoned", func() {
					// cordon all nodes but one
					for _, cordonNode := range nodesWithLabels[:len(nodesWithLabels)-1] {
						cordonNodes = append(cordonNodes, cordonNode)
						cmd := fmt.Sprintf("kubectl cordon %v", cordonNode)
						_, _, err := tests.Run(cmd)
						Expect(err).ToNot(HaveOccurred())
					}
				})

				// Create a cluster in a namespace we'll delete after the test
				err := env.CreateNamespace(namespace)
				Expect(err).ToNot(HaveOccurred())
				AssertCreateCluster(namespace, clusterName, sampleFile, env)

				By("waiting for the jobs to be removed", func() {
					// Wait for jobs to be removed
					timeout := 180
					Eventually(func() (int, error) {
						podList, err := env.GetPodList(namespace)
						return len(podList.Items), err
					}, timeout).Should(BeEquivalentTo(3))
				})

				// Load test data
				oldPrimary := clusterName + "-1"
				AssertCreateTestData(namespace, clusterName, "test")

				// We create a mapping between the pod names and the UIDs of
				// their volumes. We do not expect the UIDs to change.
				// We take advantage of the fact that related PVCs and Pods have
				// the same name.
				podList, err := env.GetClusterPodList(namespace, clusterName)
				pvcUIDMap := make(map[string]types.UID)
				for _, pod := range podList.Items {
					pvcNamespacedName := types.NamespacedName{
						Namespace: namespace,
						Name:      pod.Name,
					}
					pvc := corev1.PersistentVolumeClaim{}
					err = env.Client.Get(env.Ctx, pvcNamespacedName, &pvc)
					Expect(err).ToNot(HaveOccurred())
					pvcUIDMap[pod.Name] = pvc.GetUID()
				}

				// We uncordon a cordoned node, so there will be a node for the PVCs
				// to move to.
				By(fmt.Sprintf("uncordon one more node '%v'", cordonNodes[0]), func() {
					cmd := fmt.Sprintf("kubectl uncordon %v", cordonNodes[0])
					_, _, err = tests.Run(cmd)
					Expect(err).ToNot(HaveOccurred())
				})

				// Drain the node containing the primary pod and store the list of running pods
				podsOnPrimaryNode := drainPrimaryNode(namespace, clusterName)

				By("verifying failover after drain", func() {
					timeout := 180
					// Expect a failover to have happened
					Eventually(func() (string, error) {
						pod, err := env.GetClusterPrimary(namespace, clusterName)
						return pod.Name, err
					}, timeout).ShouldNot(BeEquivalentTo(oldPrimary))
				})

				By("check new pods use old pvcs", func() {
					// Ensure evicted pods have restarted and are running.
					// one of them could have become the new primary.
					timeout := 300
					for _, podName := range podsOnPrimaryNode {
						namespacedName := types.NamespacedName{
							Namespace: namespace,
							Name:      podName,
						}
						Eventually(func() (bool, error) {
							pod := corev1.Pod{}
							err := env.Client.Get(env.Ctx, namespacedName, &pod)
							return utils.IsPodActive(pod) && utils.IsPodReady(pod), err
						}, timeout).Should(BeTrue())

						pod := corev1.Pod{}
						err = env.Client.Get(env.Ctx, namespacedName, &pod)
						// Check that the PVC UID hasn't changed
						pvc := corev1.PersistentVolumeClaim{}
						err = env.Client.Get(env.Ctx, namespacedName, &pvc)
						Expect(pvc.GetUID(), err).To(BeEquivalentTo(pvcUIDMap[podName]))
					}
				})

				// Expect the (previously created) test data to be available

				primary, err := env.GetClusterPrimary(namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				AssertDataExpectedCount(namespace, primary.GetName(), "test", 2)
				assertClusterStandbysAreStreaming(namespace, clusterName)
			})
		})
	})

	Context("Maintenance on, reuse pvc off", func() {
		// Set unique namespace
		const namespace = "drain-node-e2e-pvc-off-single-node"
		const sampleFile = fixturesDir + "/drain-node/cluster-drain-node-pvc-off.yaml"
		const clusterName = "cluster-drain-node"

		JustAfterEach(func() {
			if CurrentSpecReport().Failed() {
				env.DumpClusterEnv(namespace, clusterName,
					"out/"+CurrentSpecReport().LeafNodeText+".log")
			}
		})
		AfterEach(func() {
			err := env.DeleteNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
		})

		// With reusePVC set to off, draining a node should create new pods
		// on different nodes. We expect to see the cluster pods having
		// all different names from the initial ones after the drain.
		It("drains the primary pod's node, when all the pods are on a single node", func() {
			// We leave a single node uncordoned, so all the pods we create
			// will go there
			By("leaving a single uncordoned", func() {
				for _, cordonNode := range nodesWithLabels[:len(nodesWithLabels)-1] {
					cmd := fmt.Sprintf("kubectl cordon %v", cordonNode)
					_, _, err := tests.Run(cmd)
					Expect(err).ToNot(HaveOccurred())
				}
			})

			// Create a cluster in a namespace we'll delete after the test
			err := env.CreateNamespace(namespace)
			Expect(err).ToNot(HaveOccurred())
			AssertCreateCluster(namespace, clusterName, sampleFile, env)

			// Avoid pod from init jobs interfering with the tests
			By("waiting for the jobs to be removed", func() {
				// Wait for jobs to be removed
				timeout := 180
				Eventually(func() (int, error) {
					podList, err := env.GetPodList(namespace)
					return len(podList.Items), err
				}, timeout).Should(BeEquivalentTo(3))
			})

			// Retrieve the names of the current pods. All of them should
			// not exists anymore after the drain
			var podsBeforeDrain []string
			By("retrieving the current pods' names", func() {
				podList, err := env.GetClusterPodList(namespace, clusterName)
				Expect(err).ToNot(HaveOccurred())
				for _, pod := range podList.Items {
					podsBeforeDrain = append(podsBeforeDrain, pod.Name)
				}
			})

			// Load test data
			AssertCreateTestData(namespace, clusterName, "test")

			// We uncordon a cordoned node. New pods can go there.
			By("uncordon node for pod failover", func() {
				cmd := fmt.Sprintf("kubectl uncordon %v", nodesWithLabels[0])
				_, _, err := tests.Run(cmd)
				Expect(err).ToNot(HaveOccurred())
			})

			// Drain the node containing the primary pod. Pods should be moved
			// to the node we've just uncordoned
			drainPrimaryNode(namespace, clusterName)

			// Expect pods to be recreated and to be ready
			AssertClusterIsReady(namespace, clusterName, 600, env)

			// Expect pods to be running on the uncordoned node and to have new names
			By("verifying cluster pods changed names", func() {
				timeout := 300
				Eventually(func() bool {
					matchingNames := 0
					podList, err := env.GetClusterPodList(namespace, clusterName)
					if err != nil {
						return false
					}
					for _, pod := range podList.Items {
						// compare the old pod list with the current pod names
						for _, oldName := range podsBeforeDrain {
							if pod.GetName() == oldName {
								matchingNames++
							}
						}
					}
					return len(podList.Items) == 3 && matchingNames == 0
				}, timeout).Should(BeTrue())
			})

			// Expect the (previously created) test data to be available
			primary, err := env.GetClusterPrimary(namespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			AssertDataExpectedCount(namespace, primary.GetName(), "test", 2)
			assertClusterStandbysAreStreaming(namespace, clusterName)
			uncordonAllNodes()
		})
	})
})

// drainPrimaryNode drains the node containing the primary pod.
// It returns the names of the pods that were running on that node
func drainPrimaryNode(namespace string, clusterName string) []string {
	var primaryNode string
	var podNames []string
	By("identifying primary node and draining", func() {
		pod, err := env.GetClusterPrimary(namespace, clusterName)
		Expect(err).ToNot(HaveOccurred())
		primaryNode = pod.Spec.NodeName

		// Gather the pods running on this node
		podList, err := env.GetClusterPodList(namespace, clusterName)
		Expect(err).ToNot(HaveOccurred())
		for _, pod := range podList.Items {
			if pod.Spec.NodeName == primaryNode {
				podNames = append(podNames, pod.Name)
			}
		}

		// Draining the primary pod's node
		timeout := 900
		// should set a timeout otherwise will hang forever
		var stdout, stderr string
		Eventually(func() error {
			cmd := fmt.Sprintf("kubectl drain %v --ignore-daemonsets --delete-local-data --force --timeout=%ds",
				primaryNode, timeout)
			stdout, stderr, err = tests.RunUnchecked(cmd)
			return err
		}, timeout).ShouldNot(HaveOccurred(), fmt.Sprintf("stdout: %s, stderr: %s", stdout, stderr))
	})
	By("ensuring no cluster pod is still running on the drained node", func() {
		timeout := 60
		Eventually(func() ([]string, error) {
			var usedNodes []string
			podList, err := env.GetClusterPodList(namespace, clusterName)
			for _, pod := range podList.Items {
				usedNodes = append(usedNodes, pod.Spec.NodeName)
			}
			return usedNodes, err
		}, timeout).ShouldNot(ContainElement(primaryNode))
	})

	return podNames
}

func uncordonAllNodes() {
	nodeList, err := env.GetNodeList()
	Expect(err).ToNot(HaveOccurred())
	// uncordoning all nodes
	for _, node := range nodeList.Items {
		command := fmt.Sprintf("kubectl uncordon %v", node.Name)
		_, _, err := tests.Run(command)
		Expect(err).ToNot(HaveOccurred())
	}
}
