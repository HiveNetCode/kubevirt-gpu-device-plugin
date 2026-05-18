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

	Describe("createDevicePlugins marks busy-group GPUs Unhealthy", func() {
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

		It("keeps busy GPUs in the pool but marks them Unhealthy", func() {
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

			Expect(devsByID).To(HaveKey("0000:23:00.0"),
				"a busy GPU must stay in the advertised pool so kubelet's capacity accounting is correct and the existing-pod allocation is preserved")
			Expect(devsByID["0000:23:00.0"]).To(Equal(pluginapi.Unhealthy),
				"a busy GPU must be Unhealthy so kubelet does not hand the same PCI ID to a new pod — which would crashloop in qemu with /dev/vfio/<group> EBUSY")

			Expect(devsByID).To(HaveKeyWithValue("0000:24:00.0", pluginapi.Healthy),
				"a free GPU must remain Healthy and available for allocation")
		})
	})
})
