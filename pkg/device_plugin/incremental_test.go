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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

var _ = Describe("incremental late-binding discovery", func() {

	Describe("GenericDevicePlugin.AddDevice", func() {
		It("appends a new device and signals ListAndWatch via the update channel", func() {
			dp := NewGenericDevicePlugin("test", "/dev/vfio/", []*pluginapi.Device{
				{ID: "0000:01:00.0", Health: pluginapi.Healthy},
			})

			dp.AddDevice(&pluginapi.Device{ID: "0000:23:00.0", Health: pluginapi.Healthy})

			Expect(dp.snapshotDevs()).To(HaveLen(2))
			ids := make([]string, 0, 2)
			for _, d := range dp.snapshotDevs() {
				ids = append(ids, d.ID)
			}
			Expect(ids).To(ConsistOf("0000:01:00.0", "0000:23:00.0"))

			select {
			case <-dp.update:
				// good
			default:
				Fail("AddDevice must signal ListAndWatch via the update channel")
			}
		})

		It("is idempotent: re-adding the same device ID does not grow the pool", func() {
			dp := NewGenericDevicePlugin("test", "/dev/vfio/", []*pluginapi.Device{
				{ID: "0000:01:00.0", Health: pluginapi.Healthy},
			})

			dp.AddDevice(&pluginapi.Device{ID: "0000:01:00.0", Health: pluginapi.Healthy})

			Expect(dp.snapshotDevs()).To(HaveLen(1),
				"second AddDevice for the same ID must be a no-op so the watcher can safely re-emit a BDF")
		})
	})

	Describe("onLateVfioBinding", func() {
		var (
			origReadLink     func(string, string, string) (string, error)
			origReadID       func(string, string, string) (string, error)
			origReadNUMA     func(string, string) (int64, error)
			origDeviceMap    map[string][]NvidiaGpuDevice
			origIommuMap     map[string][]NvidiaGpuDevice
			origBdfToIommu   map[string]string
			origRegistry     map[string]*GenericDevicePlugin
		)

		BeforeEach(func() {
			origReadLink = readLink
			origReadID = readIDFromFile
			origReadNUMA = readNUMANode
			origDeviceMap = deviceMap
			origIommuMap = iommuMap
			origBdfToIommu = bdfToIommuMap
			origRegistry = pluginRegistry

			deviceMap = map[string][]NvidiaGpuDevice{}
			iommuMap = map[string][]NvidiaGpuDevice{}
			bdfToIommuMap = map[string]string{}
			pluginRegistry = map[string]*GenericDevicePlugin{}

			readLink = func(_, _, link string) (string, error) {
				switch link {
				case "driver":
					return "vfio-pci", nil
				case "iommu_group":
					return "73", nil
				}
				return "", nil
			}
			readIDFromFile = func(_, _, prop string) (string, error) {
				switch prop {
				case "vendor":
					return nvidiaVendorID, nil
				case "device":
					return "2684", nil
				case "class":
					return "030000", nil
				}
				return "", nil
			}
			readNUMANode = func(_, _ string) (int64, error) { return 1, nil }
		})

		AfterEach(func() {
			readLink = origReadLink
			readIDFromFile = origReadID
			readNUMANode = origReadNUMA
			deviceMap = origDeviceMap
			iommuMap = origIommuMap
			bdfToIommuMap = origBdfToIommu
			pluginRegistry = origRegistry
		})

		It("registers the BDF in the maps and pushes it to the live plugin", func() {
			dp := NewGenericDevicePlugin("AD102_GEFORCE_RTX_4090", "/dev/vfio/", []*pluginapi.Device{
				{ID: "0000:01:00.0", Health: pluginapi.Healthy},
			})
			registerPlugin("2684", dp)

			onLateVfioBinding("0000:23:00.0")

			Expect(bdfToIommuMap).To(HaveKeyWithValue("0000:23:00.0", "73"),
				"the new BDF must appear in bdfToIommuMap so Allocate() can resolve it")
			Expect(deviceMap["2684"]).To(HaveLen(1))
			Expect(deviceMap["2684"][0].addr).To(Equal("0000:23:00.0"))

			snap := dp.snapshotDevs()
			Expect(snap).To(HaveLen(2),
				"the plugin's pool must grow without a process restart — that is the whole point of v3")
			ids := []string{snap[0].ID, snap[1].ID}
			Expect(ids).To(ConsistOf("0000:01:00.0", "0000:23:00.0"))

			select {
			case <-dp.update:
				// good — ListAndWatch will re-publish
			default:
				Fail("onLateVfioBinding must signal the plugin's update channel so kubelet sees the new device")
			}
		})

		It("is a no-op for a BDF already in the maps", func() {
			dp := NewGenericDevicePlugin("AD102_GEFORCE_RTX_4090", "/dev/vfio/", []*pluginapi.Device{
				{ID: "0000:23:00.0", Health: pluginapi.Healthy},
			})
			registerPlugin("2684", dp)
			bdfToIommuMap["0000:23:00.0"] = "73"
			deviceMap["2684"] = []NvidiaGpuDevice{{addr: "0000:23:00.0", numaNode: 1}}

			onLateVfioBinding("0000:23:00.0")

			Expect(dp.snapshotDevs()).To(HaveLen(1),
				"a duplicate binding event must not double-register the device")
		})
	})
})
