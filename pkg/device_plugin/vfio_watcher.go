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
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// pciDriversBasePath is the sysfs root under which each PCI driver exposes a
// directory containing symlinks for the BDFs it owns. Overridable for tests.
var pciDriversBasePath = "/sys/bus/pci/drivers"

// nvidiaVfioBdfs returns the set of NVIDIA PCI BDFs currently bound to any
// supported VFIO driver. Overridable for tests.
var nvidiaVfioBdfs = nvidiaVfioBdfsFunc

// pciAddressRe matches the canonical PCI BDF format
// "domain:bus:device.function" — domain (4 hex), bus (2 hex), device (2 hex),
// function (1 hex). Stricter than a "0000:" prefix check, so it correctly
// rejects sysfs control entries like "bind", "unbind", "new_id", "module".
var pciAddressRe = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

// watchVfioBindings reports every NVIDIA PCI device that becomes bound to a
// supported VFIO driver after the initial discovery, by invoking onBound for
// each new BDF. It does not exit the process — the plugin keeps running and
// the caller is expected to publish the new device through its live
// ListAndWatch stream.
//
// Earlier versions of this watcher closed a stop channel on the first new
// binding to trigger a container restart. That worked for the original
// symptom but introduced a subtler bug on multi-tenant GPU nodes: kubelet
// had to reconcile its pre-restart device set against the post-restart
// ListAndWatch frames, and during that window in-flight pod allocations
// could be dropped or double-handed-out — qemu then failed to open
// /dev/vfio/<group> with EBUSY. Live incremental updates avoid the whole
// reconciliation window.
//
// The watcher captures a baseline of currently-bound NVIDIA BDFs before
// adding the fsnotify watch and re-scans immediately after Add to close the
// race between baseline capture and watcher activation. Each new BDF is
// dispatched exactly once via the baseline set.
func watchVfioBindings(stop <-chan struct{}, onBound func(bdf string)) {
	const method = "vfio-watcher"

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("%s: cannot create fsnotify watcher: %v", method, err)
		return
	}
	defer watcher.Close()

	baseline := nvidiaVfioBdfs()

	watched := addSupportedDriverWatches(watcher)
	if len(watched) == 0 {
		log.Printf("%s: no supported VFIO driver directories present, exiting", method)
		return
	}
	log.Printf("%s: watching drivers=%v; baseline=%d NVIDIA GPU(s)", method, watched, len(baseline))

	// Re-check after Add to close the race window between baseline capture
	// and watcher activation.
	if extra := newNvidiaBdfs(baseline); len(extra) > 0 {
		log.Printf("%s: post-baseline rescan found new NVIDIA GPU(s): %s", method, joinSorted(extra))
		for _, bdf := range extra {
			onBound(bdf)
			baseline[bdf] = struct{}{}
		}
	}

	for {
		select {
		case <-stop:
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if !isNewNvidiaBinding(ev, baseline) {
				continue
			}
			bdf := filepath.Base(ev.Name)
			log.Printf("%s: new NVIDIA GPU %s bound to a VFIO driver", method, bdf)
			onBound(bdf)
			baseline[bdf] = struct{}{}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("%s: error: %v", method, err)
		}
	}
}

// addSupportedDriverWatches adds an fsnotify watch for every entry in
// supportedVfioDrivers whose directory exists, returning the names of those
// successfully watched (sorted). Drivers not loaded on the host are silently
// skipped.
func addSupportedDriverWatches(watcher *fsnotify.Watcher) []string {
	const method = "vfio-watcher"
	watched := make([]string, 0, len(supportedVfioDrivers))
	for driver := range supportedVfioDrivers {
		path := filepath.Join(pciDriversBasePath, driver)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := watcher.Add(path); err != nil {
			log.Printf("%s: cannot watch %s: %v", method, path, err)
			continue
		}
		watched = append(watched, driver)
	}
	sort.Strings(watched)
	return watched
}

// isNewNvidiaBinding reports whether ev is a Create event for a previously
// unseen NVIDIA-vendor PCI device. It rejects non-Create events, sysfs
// control entries (bind/unbind/...), already-baselined BDFs, and devices
// from other vendors.
func isNewNvidiaBinding(ev fsnotify.Event, baseline map[string]struct{}) bool {
	if ev.Op&fsnotify.Create == 0 {
		return false
	}
	bdf := filepath.Base(ev.Name)
	if !isPCIAddress(bdf) {
		return false
	}
	if _, seen := baseline[bdf]; seen {
		return false
	}
	vendor, err := readIDFromFile(basePath, bdf, "vendor")
	return err == nil && vendor == nvidiaVendorID
}

// nvidiaVfioBdfsFunc is the production implementation of nvidiaVfioBdfs.
func nvidiaVfioBdfsFunc() map[string]struct{} {
	out := make(map[string]struct{})
	for driver := range supportedVfioDrivers {
		entries, err := os.ReadDir(filepath.Join(pciDriversBasePath, driver))
		if err != nil {
			continue
		}
		for _, e := range entries {
			bdf := e.Name()
			if !isPCIAddress(bdf) {
				continue
			}
			vendor, err := readIDFromFile(basePath, bdf, "vendor")
			if err == nil && vendor == nvidiaVendorID {
				out[bdf] = struct{}{}
			}
		}
	}
	return out
}

// newNvidiaBdfs returns NVIDIA BDFs currently bound to a VFIO driver but
// absent from baseline.
func newNvidiaBdfs(baseline map[string]struct{}) []string {
	var extra []string
	for bdf := range nvidiaVfioBdfs() {
		if _, ok := baseline[bdf]; !ok {
			extra = append(extra, bdf)
		}
	}
	return extra
}

// isPCIAddress reports whether s is a canonical PCI BDF
// (e.g. "0000:01:00.0"). Filters out sysfs control entries the kernel
// exposes under driver dirs (bind, unbind, new_id, module, etc.).
func isPCIAddress(s string) bool {
	return pciAddressRe.MatchString(s)
}

// joinSorted returns s with entries sorted ascending and comma-joined, so
// log output is deterministic regardless of map iteration order.
func joinSorted(s []string) string {
	cp := make([]string, len(s))
	copy(cp, s)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
