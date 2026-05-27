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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

var _ = Describe("vfio-busy", func() {

	Describe("isVfioGroupBusyFunc", func() {
		var (
			tmpDir            string
			origBasePath      string
			err               error
		)

		BeforeEach(func() {
			tmpDir, err = os.MkdirTemp("", "vfio-busy-test")
			Expect(err).ToNot(HaveOccurred())
			origBasePath = vfioGroupBasePath
			vfioGroupBasePath = tmpDir
		})

		AfterEach(func() {
			vfioGroupBasePath = origBasePath
			os.RemoveAll(tmpDir)
		})

		It("reports not busy when /dev/vfio/<group> is absent", func() {
			// A transient ENOENT (missing /dev node or wrong mount) must
			// not silently shrink the advertised pool.
			Expect(isVfioGroupBusyFunc("does-not-exist")).To(BeFalse())
		})

		It("reports not busy when the group node opens cleanly", func() {
			// A regular file stands in for a free vfio group node — it opens
			// without EBUSY, so the helper must report not busy.
			groupPath := filepath.Join(tmpDir, "42")
			Expect(os.WriteFile(groupPath, nil, 0644)).To(Succeed())
			Expect(isVfioGroupBusyFunc("42")).To(BeFalse())
		})
	})

	Describe("createDevicePlugins keeps all vfio-bound GPUs Healthy", func() {
		var (
			origIsBusy     func(string) bool
			origDeviceMap  map[string][]NvidiaGpuDevice
			origBdfToIommu map[string]string
			origStart      func(*GenericDevicePlugin) error
			started        []*GenericDevicePlugin
		)

		BeforeEach(func() {
			origDeviceMap = deviceMap
			origBdfToIommu = bdfToIommuMap

			// Two NVIDIA GPUs of the same device id sharing a single plugin:
			// 0000:23:00.0 → group 51 (busy, held by another tenant VM)
			// 0000:24:00.0 → group 73 (free)
			deviceMap = map[string][]NvidiaGpuDevice{
				"2684": {
					{addr: "0000:23:00.0", numaNode: 0},
					{addr: "0000:24:00.0", numaNode: 0},
				},
			}
			bdfToIommuMap = map[string]string{
				"0000:23:00.0": "51",
				"0000:24:00.0": "73",
			}

			origIsBusy = isVfioGroupBusy
			isVfioGroupBusy = func(group string) bool { return group == "51" }

			origStart = startDevicePlugin
			started = nil
			startDevicePlugin = func(dp *GenericDevicePlugin) error {
				started = append(started, dp)
				return nil
			}
		})

		AfterEach(func() {
			deviceMap = origDeviceMap
			bdfToIommuMap = origBdfToIommu
			isVfioGroupBusy = origIsBusy
			startDevicePlugin = origStart
		})

		It("keeps the full pool Healthy regardless of current vfio-busy state", func() {
			// createDevicePlugins blocks on the package stop channel after
			// constructing the plugins. Run it in a goroutine, give it a
			// moment to populate `started`, then unblock it.
			go createDevicePlugins()
			Eventually(func() int { return len(started) }, "2s", "20ms").
				Should(Equal(1), "a single plugin is created per device id (here 2684)")
			stop <- struct{}{}

			devsByID := map[string]string{}
			for _, d := range started[0].devs {
				devsByID[d.ID] = d.Health
			}

			// Capacity must reflect the physical hardware — platform-api
			// computes "available" as allocatable − sum(GPUs of running
			// instances), so shrinking allocatable when devices are in
			// passthrough double-subtracts and reports negative free GPUs.
			Expect(devsByID).To(HaveKeyWithValue("0000:23:00.0", pluginapi.Healthy),
				"a busy GPU must still be Healthy so node capacity reflects physical hardware; the in-use check is enforced in Allocate()")
			Expect(devsByID).To(HaveKeyWithValue("0000:24:00.0", pluginapi.Healthy),
				"a free GPU must of course be Healthy")
		})
	})

	Describe("substituteBusyDevices", func() {
		var (
			origIsBusy func(string) bool
			pool       []*pluginapi.Device
			bdfToIommu map[string]string
		)

		BeforeEach(func() {
			// Pool of three devices, all known to the plugin.
			pool = []*pluginapi.Device{
				{ID: "0000:23:00.0"},
				{ID: "0000:24:00.0"},
				{ID: "0000:25:00.0"},
			}
			bdfToIommu = map[string]string{
				"0000:23:00.0": "51",
				"0000:24:00.0": "73",
				"0000:25:00.0": "95",
			}

			origIsBusy = isVfioGroupBusy
		})

		AfterEach(func() {
			isVfioGroupBusy = origIsBusy
		})

		It("returns requested IDs unchanged when none are busy", func() {
			isVfioGroupBusy = func(string) bool { return false }
			out, err := substituteBusyDevices(pool, []string{"0000:23:00.0", "0000:24:00.0"}, bdfToIommu)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(Equal([]string{"0000:23:00.0", "0000:24:00.0"}))
		})

		It("substitutes a busy device with a free one from the pool", func() {
			// Only group 51 (BDF 0000:23:00.0) is busy.
			isVfioGroupBusy = func(g string) bool { return g == "51" }
			out, err := substituteBusyDevices(pool, []string{"0000:23:00.0"}, bdfToIommu)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(HaveLen(1))
			Expect(out[0]).ToNot(Equal("0000:23:00.0"), "the busy ID must be replaced")
			Expect(bdfToIommu[out[0]]).To(BeElementOf("73", "95"), "the substitute must be a known-free device")
		})

		It("does not pick a substitute that is already part of the response", func() {
			// 51 busy, 73 free, 95 also busy → only one free candidate, but
			// kubelet asked for two devices. We must surface an error rather
			// than return a duplicate (which would crash qemu later).
			isVfioGroupBusy = func(g string) bool { return g == "51" || g == "95" }
			_, err := substituteBusyDevices(pool, []string{"0000:23:00.0", "0000:25:00.0"}, bdfToIommu)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no free VFIO group available"))
		})

		It("keeps healthy + swap combinations consistent (free ID retained, busy ID swapped)", func() {
			// 51 busy, 73 free, 95 free. kubelet asks for [23 (busy), 24 (free)].
			// Expect: [<swap of 23 → some free other than 24>, 24]
			isVfioGroupBusy = func(g string) bool { return g == "51" }
			out, err := substituteBusyDevices(pool, []string{"0000:23:00.0", "0000:24:00.0"}, bdfToIommu)
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(HaveLen(2))
			Expect(out[1]).To(Equal("0000:24:00.0"), "a free requested ID must not be touched")
			Expect(out[0]).To(Equal("0000:25:00.0"), "the swap should pick the next free device, skipping ones already in the response")
		})
	})

	Describe("filterPreferredFreeDevices", func() {
		var origIsBusy func(string) bool

		BeforeEach(func() { origIsBusy = isVfioGroupBusy })
		AfterEach(func() { isVfioGroupBusy = origIsBusy })

		It("returns the slice unchanged when no devices are busy", func() {
			isVfioGroupBusy = func(string) bool { return false }
			in := []string{"a", "b", "c"}
			bdfToIommu := map[string]string{"a": "1", "b": "2", "c": "3"}
			Expect(filterPreferredFreeDevices(in, bdfToIommu)).To(Equal(in))
		})

		It("moves busy devices to the end so kubelet picks free ones first", func() {
			isVfioGroupBusy = func(g string) bool { return g == "1" || g == "3" }
			bdfToIommu := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
			out := filterPreferredFreeDevices([]string{"a", "b", "c", "d"}, bdfToIommu)
			Expect(out).To(Equal([]string{"b", "d", "a", "c"}))
		})
	})
})
