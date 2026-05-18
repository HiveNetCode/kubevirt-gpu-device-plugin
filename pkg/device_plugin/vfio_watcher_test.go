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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/fsnotify/fsnotify"
)

var _ = Describe("vfio-watcher", func() {

	Describe("isPCIAddress", func() {
		DescribeTable("recognises canonical BDFs and rejects sysfs control entries",
			func(in string, want bool) {
				Expect(isPCIAddress(in)).To(Equal(want))
			},
			Entry("simple BDF", "0000:01:00.0", true),
			Entry("upper bus", "0000:c1:00.1", true),
			Entry("non-zero domain", "abcd:01:00.0", true),
			Entry("max function digit", "0000:01:00.f", true),
			Entry("missing domain", "01:00.0", false),
			Entry("uppercase hex (sysfs is lowercase)", "0000:0A:00.0", false),
			Entry("control entry bind", "bind", false),
			Entry("control entry unbind", "unbind", false),
			Entry("control entry new_id", "new_id", false),
			Entry("control entry module", "module", false),
			Entry("trailing junk", "0000:01:00.0.tmp", false),
			Entry("empty", "", false),
		)
	})

	Describe("joinSorted", func() {
		It("sorts entries ascending and comma-joins them", func() {
			Expect(joinSorted([]string{"0000:c1:00.0", "0000:01:00.0", "0000:23:00.0"})).
				To(Equal("0000:01:00.0,0000:23:00.0,0000:c1:00.0"))
		})
		It("returns empty string for empty input", func() {
			Expect(joinSorted(nil)).To(Equal(""))
		})
	})

	Describe("newNvidiaBdfs", func() {
		var origNvidiaVfioBdfs func() map[string]struct{}

		BeforeEach(func() {
			origNvidiaVfioBdfs = nvidiaVfioBdfs
		})
		AfterEach(func() {
			nvidiaVfioBdfs = origNvidiaVfioBdfs
		})

		It("returns BDFs present now but absent from baseline", func() {
			nvidiaVfioBdfs = func() map[string]struct{} {
				return map[string]struct{}{
					"0000:01:00.0": {},
					"0000:23:00.0": {},
					"0000:41:00.0": {},
				}
			}
			baseline := map[string]struct{}{
				"0000:01:00.0": {},
			}
			Expect(newNvidiaBdfs(baseline)).To(ConsistOf("0000:23:00.0", "0000:41:00.0"))
		})

		It("returns nil when nothing new appeared", func() {
			nvidiaVfioBdfs = func() map[string]struct{} {
				return map[string]struct{}{"0000:01:00.0": {}}
			}
			baseline := map[string]struct{}{"0000:01:00.0": {}}
			Expect(newNvidiaBdfs(baseline)).To(BeEmpty())
		})
	})

	Describe("isNewNvidiaBinding", func() {
		var origReadID func(string, string, string) (string, error)

		BeforeEach(func() {
			origReadID = readIDFromFile
		})
		AfterEach(func() {
			readIDFromFile = origReadID
		})

		It("returns true for a new NVIDIA BDF created event", func() {
			readIDFromFile = func(_, _, _ string) (string, error) { return nvidiaVendorID, nil }
			ev := fsnotify.Event{Name: "/sys/.../0000:01:00.0", Op: fsnotify.Create}
			Expect(isNewNvidiaBinding(ev, map[string]struct{}{})).To(BeTrue())
		})

		It("returns false for non-Create events", func() {
			readIDFromFile = func(_, _, _ string) (string, error) { return nvidiaVendorID, nil }
			ev := fsnotify.Event{Name: "/sys/.../0000:01:00.0", Op: fsnotify.Remove}
			Expect(isNewNvidiaBinding(ev, map[string]struct{}{})).To(BeFalse())
		})

		It("returns false for sysfs control files", func() {
			ev := fsnotify.Event{Name: "/sys/.../bind", Op: fsnotify.Create}
			Expect(isNewNvidiaBinding(ev, map[string]struct{}{})).To(BeFalse())
		})

		It("returns false when the BDF is already in the baseline", func() {
			readIDFromFile = func(_, _, _ string) (string, error) { return nvidiaVendorID, nil }
			ev := fsnotify.Event{Name: "/sys/.../0000:01:00.0", Op: fsnotify.Create}
			baseline := map[string]struct{}{"0000:01:00.0": {}}
			Expect(isNewNvidiaBinding(ev, baseline)).To(BeFalse())
		})

		It("returns false for non-NVIDIA vendor", func() {
			readIDFromFile = func(_, _, _ string) (string, error) { return "8086", nil }
			ev := fsnotify.Event{Name: "/sys/.../0000:01:00.0", Op: fsnotify.Create}
			Expect(isNewNvidiaBinding(ev, map[string]struct{}{})).To(BeFalse())
		})

		It("returns false when vendor read fails", func() {
			readIDFromFile = func(_, _, _ string) (string, error) { return "", os.ErrNotExist }
			ev := fsnotify.Event{Name: "/sys/.../0000:01:00.0", Op: fsnotify.Create}
			Expect(isNewNvidiaBinding(ev, map[string]struct{}{})).To(BeFalse())
		})
	})

	Describe("watchVfioBindings", func() {
		var (
			tmpDir              string
			origDriversBasePath string
			origNvidiaVfioBdfs  func() map[string]struct{}
			origReadID          func(string, string, string) (string, error)
		)

		BeforeEach(func() {
			var err error
			tmpDir, err = os.MkdirTemp("", "vfio-watcher-test")
			Expect(err).NotTo(HaveOccurred())
			Expect(os.MkdirAll(filepath.Join(tmpDir, "vfio-pci"), 0o755)).To(Succeed())

			origDriversBasePath = pciDriversBasePath
			origNvidiaVfioBdfs = nvidiaVfioBdfs
			origReadID = readIDFromFile
			pciDriversBasePath = tmpDir
		})

		AfterEach(func() {
			pciDriversBasePath = origDriversBasePath
			nvidiaVfioBdfs = origNvidiaVfioBdfs
			readIDFromFile = origReadID
			Expect(os.RemoveAll(tmpDir)).To(Succeed())
		})

		It("invokes the callback when a new NVIDIA BDF appears after startup", func() {
			// Baseline captured as empty; new BDF will be created after the
			// goroutine starts.
			nvidiaVfioBdfs = func() map[string]struct{} { return map[string]struct{}{} }
			readIDFromFile = func(_, _, _ string) (string, error) { return nvidiaVendorID, nil }

			origCb := onNewVfioBdf
			defer func() { onNewVfioBdf = origCb }()
			seen := make(chan string, 4)
			onNewVfioBdf = func(bdf string) { seen <- bdf }

			stop := make(chan struct{})
			defer close(stop)
			go watchVfioBindings(stop)

			// Give the watcher a moment to set up its fsnotify Add.
			time.Sleep(150 * time.Millisecond)

			// Simulate a binding by creating a BDF entry in the watched dir.
			Expect(os.WriteFile(filepath.Join(tmpDir, "vfio-pci", "0000:01:00.0"), nil, 0o644)).To(Succeed())

			Eventually(seen, "2s").Should(Receive(Equal("0000:01:00.0")),
				"watcher must call onNewVfioBdf so the plugin can incrementally publish the new GPU — without exiting the process")
		})

		It("invokes the callback on the post-baseline rescan when a binding races in", func() {
			// First call (baseline) returns empty; second call (post-Add
			// rescan) returns a new BDF — simulating vfio-manager binding
			// during the small window between the two reads.
			calls := 0
			nvidiaVfioBdfs = func() map[string]struct{} {
				calls++
				if calls == 1 {
					return map[string]struct{}{}
				}
				return map[string]struct{}{"0000:01:00.0": {}}
			}

			origCb := onNewVfioBdf
			defer func() { onNewVfioBdf = origCb }()
			seen := make(chan string, 4)
			onNewVfioBdf = func(bdf string) { seen <- bdf }

			stop := make(chan struct{})
			defer close(stop)
			go watchVfioBindings(stop)

			Eventually(seen, "2s").Should(Receive(Equal("0000:01:00.0")),
				"watcher must call onNewVfioBdf for BDFs that bound during the baseline-to-Add race window")
		})

		It("ignores Create events for sysfs control entries", func() {
			nvidiaVfioBdfs = func() map[string]struct{} { return map[string]struct{}{} }
			readIDFromFile = func(_, _, _ string) (string, error) { return nvidiaVendorID, nil }

			origCb := onNewVfioBdf
			defer func() { onNewVfioBdf = origCb }()
			seen := make(chan string, 4)
			onNewVfioBdf = func(bdf string) { seen <- bdf }

			stop := make(chan struct{})
			defer close(stop)
			go watchVfioBindings(stop)
			time.Sleep(150 * time.Millisecond)

			Expect(os.WriteFile(filepath.Join(tmpDir, "vfio-pci", "bind"), nil, 0o644)).To(Succeed())

			Consistently(seen, "300ms").ShouldNot(Receive())
		})

		It("ignores Create events for non-NVIDIA vendor devices", func() {
			nvidiaVfioBdfs = func() map[string]struct{} { return map[string]struct{}{} }
			readIDFromFile = func(_, _, _ string) (string, error) { return "8086", nil }

			origCb := onNewVfioBdf
			defer func() { onNewVfioBdf = origCb }()
			seen := make(chan string, 4)
			onNewVfioBdf = func(bdf string) { seen <- bdf }

			stop := make(chan struct{})
			defer close(stop)
			go watchVfioBindings(stop)
			time.Sleep(150 * time.Millisecond)

			Expect(os.WriteFile(filepath.Join(tmpDir, "vfio-pci", "0000:01:00.0"), nil, 0o644)).To(Succeed())

			Consistently(seen, "300ms").ShouldNot(Receive())
		})

		It("exits cleanly when no supported VFIO driver dirs exist", func() {
			Expect(os.RemoveAll(filepath.Join(tmpDir, "vfio-pci"))).To(Succeed())
			nvidiaVfioBdfs = func() map[string]struct{} { return map[string]struct{}{} }

			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				watchVfioBindings(stop)
				close(done)
			}()

			Eventually(done, "1s").Should(BeClosed())
			Expect(stop).NotTo(BeClosed())
		})
	})
})
