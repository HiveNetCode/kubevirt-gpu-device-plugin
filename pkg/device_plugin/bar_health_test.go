/*
 * Copyright (c) 2019-2026, NVIDIA CORPORATION. All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions
 * are met:
 *  * Redistributions of source code must retain the above copyright
 *    notice, this list of conditions and the following disclaimer.
 *  * Redistributions in binary form must reproduce the above copyright
 *    notice, this list of conditions and the following disclaimer in the
 *    documentation and/or other materials provided with the distribution.
 *  * Neither the name of NVIDIA CORPORATION nor the names of its
 *    contributors may be used to endorse or promote products derived
 *    from this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS ``AS IS'' AND ANY
 * EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR
 * PURPOSE ARE DISCLAIMED.  IN NO EVENT SHALL THE COPYRIGHT OWNER OR
 * CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
 * EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
 * PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
 * PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY
 * OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */

package device_plugin

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Every spec in the suite runs with BAR1 reported as assigned unless it
// overrides hasAssignedBAR1 itself: most specs exercise fake BDFs that have
// no sysfs entry on the test machine, and the production default would
// otherwise report every one of them broken.
var _ = BeforeEach(func() {
	hasAssignedBAR1 = func(string) bool { return true }
})

var _ = AfterEach(func() {
	hasAssignedBAR1 = hasAssignedBAR1Func
})

var _ = Describe("bar-health", func() {

	Describe("hasAssignedBAR1Func", func() {
		var (
			tmpDir       string
			origBasePath string
		)

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "bar-health-test")
			Expect(err).ToNot(HaveOccurred())
			origBasePath = basePath
			basePath = tmpDir
		})

		AfterEach(func() {
			basePath = origBasePath
			os.RemoveAll(tmpDir)
		})

		makeDevice := func(bdf string, resources ...string) {
			dir := filepath.Join(tmpDir, bdf)
			Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
			for _, name := range resources {
				Expect(os.WriteFile(filepath.Join(dir, name), nil, 0o644)).To(Succeed())
			}
		}

		It("reports true for a device whose BAR1 is assigned", func() {
			makeDevice("0000:65:00.0", "resource", "resource0", "resource1", "resource3")
			Expect(hasAssignedBAR1Func("0000:65:00.0")).To(BeTrue())
		})

		It("reports false for a device whose BAR1 the kernel did not assign", func() {
			// The failed-assignment signature: the device directory and
			// resource0 exist, resource1 does not.
			makeDevice("0000:66:00.0", "resource", "resource0", "resource1_resize")
			Expect(hasAssignedBAR1Func("0000:66:00.0")).To(BeFalse())
		})

		It("reports false for a device absent from sysfs", func() {
			Expect(hasAssignedBAR1Func("0000:ff:00.0")).To(BeFalse())
		})
	})

	Describe("advertisedHealth", func() {
		var origIsBusy func(string) bool

		BeforeEach(func() {
			origIsBusy = isVfioGroupBusy
			isVfioGroupBusy = func(string) bool { return false }
		})

		AfterEach(func() {
			isVfioGroupBusy = origIsBusy
		})

		It("advertises a free BAR-broken GPU Unhealthy", func() {
			hasAssignedBAR1 = func(bdf string) bool { return bdf != "bad" }
			Expect(advertisedHealth("good", "50")).To(Equal(pluginapi.Healthy))
			Expect(advertisedHealth("bad", "51")).To(Equal(pluginapi.Unhealthy))
		})

		It("keeps a VM-held BAR-broken GPU Healthy so availability is not double-subtracted", func() {
			hasAssignedBAR1 = func(string) bool { return false }
			isVfioGroupBusy = func(g string) bool { return g == "51" }
			Expect(advertisedHealth("bad", "51")).To(Equal(pluginapi.Healthy))
			Expect(advertisedHealth("bad", "52")).To(Equal(pluginapi.Unhealthy))
		})
	})

	Describe("pushBarHealthTransitions", func() {
		var (
			origIsBusy     func(string) bool
			origNodeExists func(string) bool
			origIommuMap   func() map[string]string
		)

		BeforeEach(func() {
			origIsBusy = isVfioGroupBusy
			origNodeExists = vfioGroupNodeExists
			origIommuMap = returnBdfToIommuMap
			isVfioGroupBusy = func(string) bool { return false }
			vfioGroupNodeExists = func(string) bool { return true }
			returnBdfToIommuMap = func() map[string]string {
				return map[string]string{
					"0000:65:00.0": "50",
					"0000:66:00.0": "51",
				}
			}
		})

		AfterEach(func() {
			isVfioGroupBusy = origIsBusy
			vfioGroupNodeExists = origNodeExists
			returnBdfToIommuMap = origIommuMap
		})

		newPlugin := func() *GenericDevicePlugin {
			return NewGenericDevicePlugin("FAKE_GPU", "/dev/vfio/", []*pluginapi.Device{
				{ID: "0000:65:00.0", Health: pluginapi.Healthy},
				{ID: "0000:66:00.0", Health: pluginapi.Healthy},
			})
		}

		// run executes one pass in a goroutine (its channel sends block until
		// received) and returns a channel closed when the pass finishes.
		run := func(dpi *GenericDevicePlugin, done <-chan struct{}) <-chan struct{} {
			finished := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				dpi.pushBarHealthTransitions(done)
				close(finished)
			}()
			return finished
		}

		setHealth := func(dpi *GenericDevicePlugin, idx int, health string) {
			dpi.devsMu.Lock()
			dpi.devs[idx].Health = health
			dpi.devsMu.Unlock()
		}

		It("streams a device's health transitions and settles once applied", func() {
			dpi := newPlugin()
			hasAssignedBAR1 = func(bdf string) bool { return bdf != "0000:66:00.0" }
			done := make(chan struct{})
			defer close(done)

			finished := run(dpi, done)
			var got string
			Eventually(dpi.unhealthy).Should(Receive(&got))
			Expect(got).To(Equal("0000:66:00.0"))
			Eventually(finished).Should(BeClosed())

			// Apply the transition the way ListAndWatch would; a repeat pass
			// then has nothing to send. Called synchronously: a stray send
			// would block it forever and fail the spec by timeout.
			setHealth(dpi, 1, pluginapi.Unhealthy)
			dpi.pushBarHealthTransitions(done)

			// BAR1 coming back (the device was fixed and rescanned) flows
			// through the healthy channel, because this checker's own
			// verdict is what broke the device.
			hasAssignedBAR1 = func(string) bool { return true }
			finished = run(dpi, done)
			Eventually(dpi.healthy).Should(Receive(&got))
			Expect(got).To(Equal("0000:66:00.0"))
			Eventually(finished).Should(BeClosed())
		})

		It("re-asserts Unhealthy when another producer flips a still-broken device back", func() {
			dpi := newPlugin()
			hasAssignedBAR1 = func(bdf string) bool { return bdf != "0000:66:00.0" }
			done := make(chan struct{})
			defer close(done)

			finished := run(dpi, done)
			var got string
			Eventually(dpi.unhealthy).Should(Receive(&got))
			Eventually(finished).Should(BeClosed())
			setHealth(dpi, 1, pluginapi.Unhealthy)

			// An fsnotify Create event re-marks the device Healthy while its
			// BAR1 is still unassigned; the next pass flips it back.
			setHealth(dpi, 1, pluginapi.Healthy)
			finished = run(dpi, done)
			Eventually(dpi.unhealthy).Should(Receive(&got))
			Expect(got).To(Equal("0000:66:00.0"))
			Eventually(finished).Should(BeClosed())
		})

		It("never resurrects a device the fsnotify healthCheck latched Unhealthy", func() {
			// The device was unbound from vfio-pci: its /dev/vfio/<group>
			// node is gone (which is what latched it Unhealthy), while its
			// BAR1 sysfs files persist. The BAR checker must leave it alone.
			// Synchronous call: a stray healthy send would block forever and
			// fail the spec by timeout.
			dpi := newPlugin()
			setHealth(dpi, 1, pluginapi.Unhealthy)
			vfioGroupNodeExists = func(string) bool { return false }
			done := make(chan struct{})
			defer close(done)

			dpi.pushBarHealthTransitions(done)
			dpi.pushBarHealthTransitions(done)
		})

		It("defers a broken device's Unhealthy while a VM holds it and sends it on release", func() {
			dpi := newPlugin()
			hasAssignedBAR1 = func(bdf string) bool { return bdf != "0000:66:00.0" }
			isVfioGroupBusy = func(g string) bool { return g == "51" }
			done := make(chan struct{})
			defer close(done)

			// Held: no event (synchronous call would park on a send).
			dpi.pushBarHealthTransitions(done)

			// Released: the deferred Unhealthy goes out.
			isVfioGroupBusy = func(string) bool { return false }
			finished := run(dpi, done)
			var got string
			Eventually(dpi.unhealthy).Should(Receive(&got))
			Expect(got).To(Equal("0000:66:00.0"))
			Eventually(finished).Should(BeClosed())
		})

		It("abandons undelivered sends when done closes", func() {
			dpi := newPlugin()
			hasAssignedBAR1 = func(string) bool { return false }
			done := make(chan struct{})

			finished := run(dpi, done)
			// No one receives: the pass parks on its first send until done
			// closes, then returns without delivering.
			Consistently(finished).ShouldNot(BeClosed())
			close(done)
			Eventually(finished).Should(BeClosed())
		})
	})

	Describe("barHealthCheck", func() {
		It("re-evaluates on every tick and stops when done closes", func() {
			dpi := NewGenericDevicePlugin("FAKE_GPU", "/dev/vfio/", []*pluginapi.Device{
				{ID: "0000:65:00.0", Health: pluginapi.Healthy},
			})
			// The stub is installed before the goroutine starts and never
			// reassigned while it runs; the flip goes through the atomic.
			var broken atomic.Bool
			hasAssignedBAR1 = func(string) bool { return !broken.Load() }

			done := make(chan struct{})
			finished := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				dpi.barHealthCheck(done, 2*time.Millisecond)
				close(finished)
			}()

			broken.Store(true)
			var got string
			Eventually(dpi.unhealthy).Should(Receive(&got))
			Expect(got).To(Equal("0000:65:00.0"))

			close(done)
			Eventually(finished).Should(BeClosed())
		})
	})

	Describe("unusable-device substitution for broken GPUs", func() {
		var origIsBusy func(string) bool

		pool := []*pluginapi.Device{
			{ID: "0000:65:00.0"},
			{ID: "0000:66:00.0"},
			{ID: "0000:67:00.0"},
		}
		bdfToIommu := map[string]string{
			"0000:65:00.0": "50",
			"0000:66:00.0": "51",
			"0000:67:00.0": "52",
		}

		BeforeEach(func() {
			origIsBusy = isVfioGroupBusy
			isVfioGroupBusy = func(string) bool { return false }
		})

		AfterEach(func() {
			isVfioGroupBusy = origIsBusy
		})

		It("swaps a requested BAR-broken device for a usable one", func() {
			hasAssignedBAR1 = func(bdf string) bool { return bdf != "0000:66:00.0" }
			out, err := substituteUnusableDevices(pool, []string{"0000:66:00.0", "0000:65:00.0"}, bdfToIommu)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal([]string{"0000:67:00.0", "0000:65:00.0"}))
		})

		It("errors when every candidate is broken", func() {
			hasAssignedBAR1 = func(string) bool { return false }
			_, err := substituteUnusableDevices(pool, []string{"0000:65:00.0"}, bdfToIommu)
			Expect(err).To(HaveOccurred())
		})

		It("moves broken devices to the back of the preferred pool", func() {
			hasAssignedBAR1 = func(bdf string) bool { return bdf != "0000:65:00.0" }
			out := filterPreferredUsableDevices([]string{"0000:65:00.0", "0000:66:00.0", "0000:67:00.0"}, bdfToIommu)
			Expect(out).To(Equal([]string{"0000:66:00.0", "0000:67:00.0", "0000:65:00.0"}))
		})
	})
})
