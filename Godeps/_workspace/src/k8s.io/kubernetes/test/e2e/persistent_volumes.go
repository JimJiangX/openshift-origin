/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/testapi"
	"k8s.io/kubernetes/pkg/api/unversioned"
	client "k8s.io/kubernetes/pkg/client/unversioned"
)

// This test needs privileged containers, which are disabled by default.  Run
// the test with "go run hack/e2e.go ... --ginkgo.focus=[Feature:Volumes]"
var _ = Describe("PersistentVolumes [Feature:Volumes]", func() {
	framework := NewDefaultFramework("pv")
	var c *client.Client
	var ns string

	BeforeEach(func() {
		c = framework.Client
		ns = framework.Namespace.Name
	})

	It("NFS volume can be created, bound, retrieved, unbound, and used by a pod", func() {
		config := VolumeTestConfig{
			namespace:   ns,
			prefix:      "nfs",
			serverImage: "gcr.io/google_containers/volume-nfs:0.4",
			serverPorts: []int{2049},
		}

		defer func() {
			volumeTestCleanup(c, config)
		}()

		pod := startVolumeServer(c, config)
		serverIP := pod.Status.PodIP
		Logf("NFS server IP address: %v", serverIP)

		pv := makePersistentVolume(serverIP)
		pvc := makePersistentVolumeClaim(ns)

		Logf("Creating PersistentVolume using NFS")
		pv, err := c.PersistentVolumes().Create(pv)
		Expect(err).NotTo(HaveOccurred())

		Logf("Creating PersistentVolumeClaim")
		pvc, err = c.PersistentVolumeClaims(ns).Create(pvc)
		Expect(err).NotTo(HaveOccurred())

		// allow the binder a chance to catch up.  should not be more than 20s.
		waitForPersistentVolumePhase(api.VolumeBound, c, pv.Name, 1*time.Second, 30*time.Second)

		pv, err = c.PersistentVolumes().Get(pv.Name)
		Expect(err).NotTo(HaveOccurred())
		if pv.Spec.ClaimRef == nil {
			Failf("Expected PersistentVolume to be bound, but got nil ClaimRef: %+v", pv)
		}

		Logf("Deleting PersistentVolumeClaim to trigger PV Recycling")
		err = c.PersistentVolumeClaims(ns).Delete(pvc.Name)
		Expect(err).NotTo(HaveOccurred())

		// allow the recycler a chance to catch up.  it has to perform NFS scrub, which can be slow in e2e.
		waitForPersistentVolumePhase(api.VolumeAvailable, c, pv.Name, 5*time.Second, 300*time.Second)

		pv, err = c.PersistentVolumes().Get(pv.Name)
		Expect(err).NotTo(HaveOccurred())
		if pv.Spec.ClaimRef != nil {
			Failf("Expected PersistentVolume to be unbound, but found non-nil ClaimRef: %+v", pv)
		}

		// The NFS Server pod we're using contains an index.html file
		// Verify the file was really scrubbed from the volume
		podTemplate := makeCheckPod(ns, serverIP)
		checkpod, err := c.Pods(ns).Create(podTemplate)
		expectNoError(err, "Failed to create checker pod: %v", err)
		err = waitForPodSuccessInNamespace(c, checkpod.Name, checkpod.Spec.Containers[0].Name, checkpod.Namespace)
		Expect(err).NotTo(HaveOccurred())
	})
})

func makePersistentVolume(serverIP string) *api.PersistentVolume {
	return &api.PersistentVolume{
		ObjectMeta: api.ObjectMeta{
			GenerateName: "nfs-",
		},
		Spec: api.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: api.PersistentVolumeReclaimRecycle,
			Capacity: api.ResourceList{
				api.ResourceName(api.ResourceStorage): resource.MustParse("2Gi"),
			},
			PersistentVolumeSource: api.PersistentVolumeSource{
				NFS: &api.NFSVolumeSource{
					Server:   serverIP,
					Path:     "/",
					ReadOnly: false,
				},
			},
			AccessModes: []api.PersistentVolumeAccessMode{
				api.ReadWriteOnce,
				api.ReadOnlyMany,
				api.ReadWriteMany,
			},
		},
	}
}

func makePersistentVolumeClaim(ns string) *api.PersistentVolumeClaim {
	return &api.PersistentVolumeClaim{
		ObjectMeta: api.ObjectMeta{
			GenerateName: "pvc-",
			Namespace:    ns,
		},
		Spec: api.PersistentVolumeClaimSpec{
			AccessModes: []api.PersistentVolumeAccessMode{
				api.ReadWriteOnce,
				api.ReadOnlyMany,
				api.ReadWriteMany,
			},
			Resources: api.ResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceName(api.ResourceStorage): resource.MustParse("1Gi"),
				},
			},
		},
	}
}

func makeCheckPod(ns string, nfsserver string) *api.Pod {
	// Prepare pod that mounts the NFS volume again and
	// checks that /mnt/index.html was scrubbed there
	return &api.Pod{
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Pod",
			APIVersion: testapi.Default.GroupVersion().String(),
		},
		ObjectMeta: api.ObjectMeta{
			GenerateName: "checker-",
			Namespace:    ns,
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Name:    "scrub-checker",
					Image:   "gcr.io/google_containers/busybox:1.24",
					Command: []string{"/bin/sh"},
					Args:    []string{"-c", "test ! -e /mnt/index.html || exit 1"},
					VolumeMounts: []api.VolumeMount{
						{
							Name:      "nfs-volume",
							MountPath: "/mnt",
						},
					},
				},
			},
			Volumes: []api.Volume{
				{
					Name: "nfs-volume",
					VolumeSource: api.VolumeSource{
						NFS: &api.NFSVolumeSource{
							Server: nfsserver,
							Path:   "/",
						},
					},
				},
			},
		},
	}

}
