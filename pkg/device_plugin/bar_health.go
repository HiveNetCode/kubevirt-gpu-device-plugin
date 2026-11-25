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
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// barHealthRecheckInterval is how often every advertised GPU's BAR1
// assignment is re-read from sysfs. BAR state normally only changes across a
// reboot of the host, but a remove/rescan, a late assignment failure, or a
// VM releasing a broken GPU (whose Unhealthy is deferred while it is held,
// see pushBarHealthTransitions) must not go unnoticed until the daemonset
// restarts.
const barHealthRecheckInterval = 30 * time.Second

// hasAssignedBAR1 reports whether the GPU at bdf has an address assigned to
// its BAR1 — the VRAM aperture the guest driver maps. The kernel creates
// /sys/bus/pci/devices/<bdf>/resourceN only for a BAR it assigned an address
// to, so an absent resource1 next to a present resource0 is the sysfs
// signature of a boot-time "BAR 1: failed to assign" (typically BAR space
// exhaustion on hosts with many large-BAR GPUs). A VM that receives such a
// GPU boots, but its driver cannot map the aperture (NVRM: BAR1 is 0M) and
// the GPU is unusable in the guest.
//
// The `resource` text file cannot distinguish this: BAR1 on these GPUs is
// 64-bit, so it occupies registers 1 and 2 and the register-2 line reads
// all-zero on a healthy device too. The resource1 file is the unambiguous
// signal, and it reflects BAR assignment only — it persists when the device
// is unbound from vfio-pci.
//
// A stat error other than non-existence proves nothing, so it reports the
// device usable rather than silently shrinking the advertised pool — the
// same conservatism as isVfioGroupBusy. Non-existence of the whole device
// directory means the device is gone from sysfs, which is equally unusable.
//
// Overridable for tests.
var hasAssignedBAR1 = hasAssignedBAR1Func

func hasAssignedBAR1Func(bdf string) bool {
	_, err := os.Stat(filepath.Join(basePath, bdf, "resource1"))
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return true
}

// vfioGroupNodeExists reports whether /dev/vfio/<group> is present. The
// fsnotify healthCheck marks a device Unhealthy when that node disappears
// (the device was unbound from vfio-pci), and the BAR checker must never
// undo that verdict: it sends a healthy transition only for a device whose
// group node exists. Distinct from isVfioGroupBusy, which opens the node to
// probe whether another process holds it. Overridable for tests.
var vfioGroupNodeExists = vfioGroupNodeExistsFunc

func vfioGroupNodeExistsFunc(group string) bool {
	_, err := os.Stat(filepath.Join(vfioGroupBasePath, group))
	return err == nil
}

// advertisedHealth returns the health a GPU is advertised with at discovery.
// Only for passthrough GPUs: a vGPU's address is an mdev UUID, not a PCI
// BDF.
//
// A BAR-broken GPU that a running VM currently holds is still advertised
// Healthy: marking it Unhealthy drops it from allocatable while the
// platform also counts the running instance, double-subtracting it (the
// same arithmetic that forces busy devices to stay Healthy — see the
// comment in createDevicePlugins). Allocate() and GetPreferredAllocation()
// refuse the device either way via isDeviceUnusable, and the periodic BAR
// checker marks it Unhealthy on its first pass after the VM releases it.
func advertisedHealth(bdf, iommu string) string {
	if hasAssignedBAR1(bdf) {
		return pluginapi.Healthy
	}
	if isVfioGroupBusy(iommu) {
		log.Printf("advertisedHealth %s: BAR1 is unassigned but a VM holds the device — deferring Unhealthy until it is released", bdf)
		return pluginapi.Healthy
	}
	return pluginapi.Unhealthy
}

// barHealthCheck re-evaluates every advertised GPU's BAR1 assignment each
// interval and streams the transitions to ListAndWatch until done closes.
// It runs once immediately so a plugin (re)start publishes fresh state
// without waiting out the first tick.
func (dpi *GenericDevicePlugin) barHealthCheck(done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		dpi.pushBarHealthTransitions(done)
		select {
		case <-done:
			return
		case <-ticker.C:
		}
	}
}

// pushBarHealthTransitions sends health events for devices whose advertised
// health disagrees with their BAR1 state, without ever overriding the other
// health producer: the fsnotify healthCheck owns the /dev/vfio/<group>
// node's existence, this checker owns the BAR1 verdict.
//
//   - Unhealthy is sent for a BAR-broken device advertised Healthy, unless
//     a VM currently holds its VFIO group — then the send is deferred to
//     the first pass after release, so a running instance is not
//     double-subtracted from the platform's availability (see
//     advertisedHealth). Re-evaluating against the advertised health also
//     re-asserts the verdict if another producer flips the device back.
//   - Healthy is sent for a recovered device only when this checker's own
//     verdict was what broke it (dpi.barBroken) and its /dev/vfio/<group>
//     node exists — a device the fsnotify healthCheck latched Unhealthy
//     after an unbind keeps its BAR1 sysfs files, and without these guards
//     one pass would resurrect it and kubelet would hand out a device whose
//     group node is gone.
//
// dpi.barBroken is this checker's record of its last BAR verdict per
// device, seeded from the advertised health the first time a device is
// seen (at discovery they are the same verdict). Only this goroutine — and
// tests, sequentially — touches it.
//
// Health is read under devsMu but events are sent after releasing it:
// ListAndWatch receives an event and then takes devsMu to apply it, so
// sending while holding the lock would deadlock the pair. A send blocks
// until ListAndWatch picks it up (or done closes) — with no stream
// connected, the device's recorded health stays stale, and the parked send
// delivers the same transition the moment kubelet reconnects.
func (dpi *GenericDevicePlugin) pushBarHealthTransitions(done <-chan struct{}) {
	type transition struct {
		id     string
		to     string
		broken bool
	}
	var transitions []transition
	bdfToIommu := returnBdfToIommuMap()

	dpi.devsMu.RLock()
	for _, dev := range dpi.devs {
		broken := !hasAssignedBAR1(dev.ID)
		wasBroken, seen := dpi.barBroken[dev.ID]
		if !seen {
			wasBroken = dev.Health == pluginapi.Unhealthy
		}
		iommu, haveIommu := bdfToIommu[dev.ID]

		switch {
		case broken && dev.Health == pluginapi.Healthy:
			if haveIommu && isVfioGroupBusy(iommu) {
				// Deferred: Allocate() refuses the device meanwhile.
				break
			}
			transitions = append(transitions, transition{id: dev.ID, to: pluginapi.Unhealthy, broken: broken})
			continue
		case !broken && dev.Health == pluginapi.Unhealthy && wasBroken:
			if !haveIommu || !vfioGroupNodeExists(iommu) {
				break
			}
			transitions = append(transitions, transition{id: dev.ID, to: pluginapi.Healthy, broken: broken})
			continue
		}
		dpi.barBroken[dev.ID] = broken
	}
	dpi.devsMu.RUnlock()

	// The verdict of a transitioned device is recorded only once its event
	// is delivered: an abandoned send (done closed mid-pass) must leave the
	// old verdict in place, or a recovered device's healthy event would be
	// gated off (wasBroken=false) on every later pass and the device would
	// stay Unhealthy for good.
	for _, tr := range transitions {
		ch := dpi.unhealthy
		if tr.to == pluginapi.Healthy {
			ch = dpi.healthy
		}
		select {
		case ch <- tr.id:
			dpi.barBroken[tr.id] = tr.broken
			log.Printf("barHealthCheck(%s): device %s is now %s (BAR1)", dpi.deviceName, tr.id, tr.to)
		case <-done:
			return
		}
	}
}
