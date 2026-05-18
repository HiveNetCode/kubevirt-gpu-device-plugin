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

	Describe("createIommuDeviceMap with busy-group filter", func() {
		var (
			workDir          string
			origReadLink     func(string, string, string) (string, error)
			origReadID       func(string, string, string) (string, error)
			origIsBusy       func(string) bool
			origBasePath     string
			origDeviceMap    map[string][]NvidiaGpuDevice
			origIommuMap     map[string][]NvidiaGpuDevice
			origBdfToIommu   map[string]string
		)

		BeforeEach(func() {
			var err error
			workDir, err = os.MkdirTemp("", "vfio-busy-discovery")
			Expect(err).ToNot(HaveOccurred())
			linkDir, err := os.MkdirTemp("", "vfio-busy-targets")
			Expect(err).ToNot(HaveOccurred())

			// createIommuDeviceMap uses filepath.Walk's Lstat, so directories
			// are skipped. Mirror the existing test pattern: a symlink at
			// workDir/<BDF> pointing at a real directory under linkDir lets
			// info.IsDir() return false for the entry while still being a
			// valid sysfs-like path.
			for _, bdf := range []string{"0000:23:00.0", "0000:24:00.0"} {
				target := filepath.Join(linkDir, bdf)
				Expect(os.Mkdir(target, 0755)).To(Succeed())
				Expect(os.Symlink(target, filepath.Join(workDir, bdf))).To(Succeed())
			}

			origBasePath = basePath
			basePath = workDir

			origReadLink = readLink
			readLink = func(_, addr, link string) (string, error) {
				switch link {
				case "driver":
					return "vfio-pci", nil
				case "iommu_group":
					if addr == "0000:23:00.0" {
						return "51", nil
					}
					return "73", nil
				}
				return "", nil
			}

			origReadID = readIDFromFile
			readIDFromFile = func(_, _, prop string) (string, error) {
				switch prop {
				case "vendor":
					return nvidiaVendorID, nil
				case "device":
					return "2684", nil
				}
				return "", nil
			}

			origIsBusy = isVfioGroupBusy
			isVfioGroupBusy = func(group string) bool {
				// 0000:23:00.0 (group 51) is held by another tenant VM.
				return group == "51"
			}

			origDeviceMap = deviceMap
			origIommuMap = iommuMap
			origBdfToIommu = bdfToIommuMap
		})

		AfterEach(func() {
			basePath = origBasePath
			readLink = origReadLink
			readIDFromFile = origReadID
			isVfioGroupBusy = origIsBusy
			deviceMap = origDeviceMap
			iommuMap = origIommuMap
			bdfToIommuMap = origBdfToIommu
			os.RemoveAll(workDir)
		})

		It("skips devices whose vfio group is held by another process", func() {
			createIommuDeviceMap()

			Expect(bdfToIommuMap).NotTo(HaveKey("0000:23:00.0"),
				"a device whose vfio group is held must not be advertised — otherwise kubelet hands the PCI ID to a new pod and qemu fails with /dev/vfio/<group> EBUSY")
			Expect(iommuMap).NotTo(HaveKey("51"))

			Expect(bdfToIommuMap).To(HaveKeyWithValue("0000:24:00.0", "73"),
				"a device whose vfio group is free must still be discovered")
			Expect(iommuMap).To(HaveKey("73"))
		})
	})
})
