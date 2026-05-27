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
	"fmt"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// isVfioGroupBusy reports whether /dev/vfio/<group> is currently held by
// another process (typically a running qemu-kvm / virt-launcher with a GPU
// passed through). The VFIO kernel driver returns EBUSY on the second
// open(2) of a group whose `opened` refcount is already 1, so a single
// non-blocking open is enough to distinguish "free" from "in use".
//
// Allocate() and GetPreferredAllocation() use this to swap a busy PCI ID
// for a known-free one in the plugin's pool, so a tenant VM never gets a
// /dev/vfio/<group> that is already passed through to another VM —
// preventing the "Could not open '/dev/vfio/<group>': Device or resource
// busy" qemu crashloop on plugin restart with running tenants present.
//
// Overridable for tests.
var isVfioGroupBusy = isVfioGroupBusyFunc

// vfioGroupBasePath is /dev/vfio. Overridable for tests.
var vfioGroupBasePath = "/dev/vfio"

func isVfioGroupBusyFunc(group string) bool {
	path := filepath.Join(vfioGroupBasePath, group)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EBUSY) {
			return true
		}
		// Any other error (ENOENT, EACCES, ...) means we cannot prove the
		// group is in use. Be conservative and report it as not-busy so a
		// missing /dev node or permission glitch does not silently shrink
		// the advertised pool.
		return false
	}
	_ = unix.Close(fd)
	return false
}

// substituteBusyDevices returns a copy of requested where every PCI ID
// whose IOMMU group is currently held by another process has been swapped
// for a free device from pool. The returned slice preserves ordering and
// is guaranteed not to contain duplicates.
//
// Returns an error if a busy device cannot be replaced because the pool
// has no candidate whose IOMMU group is free (and that is not already
// part of the response). Callers should surface the error to kubelet so
// the request is retried rather than producing a guaranteed-failing
// allocation.
func substituteBusyDevices(pool []*pluginapi.Device, requested []string, bdfToIommu map[string]string) ([]string, error) {
	// Two passes so a free requested ID later in the slice is not stolen
	// by the substitute step for an earlier busy one. First pass reserves
	// every requested ID we will keep (unknown or non-busy); second pass
	// picks substitutes from the remainder.
	keep := make([]bool, len(requested))
	used := make(map[string]bool, len(requested))
	for i, bdf := range requested {
		iommu, ok := bdfToIommu[bdf]
		if !ok || !isVfioGroupBusy(iommu) {
			keep[i] = true
			used[bdf] = true
		}
	}

	out := make([]string, len(requested))
	for i, bdf := range requested {
		if keep[i] {
			out[i] = bdf
			continue
		}
		sub, err := pickFreeSubstitute(pool, used, bdfToIommu)
		if err != nil {
			return nil, fmt.Errorf("cannot substitute busy device %s (iommu %s): %w", bdf, bdfToIommu[bdf], err)
		}
		out[i] = sub
		used[sub] = true
	}
	return out, nil
}

// pickFreeSubstitute returns the first device in pool whose IOMMU group is
// not currently held and that is not already in used. Returns an error if
// no candidate is available.
func pickFreeSubstitute(pool []*pluginapi.Device, used map[string]bool, bdfToIommu map[string]string) (string, error) {
	for _, d := range pool {
		if used[d.ID] {
			continue
		}
		iommu, ok := bdfToIommu[d.ID]
		if !ok {
			continue
		}
		if isVfioGroupBusy(iommu) {
			continue
		}
		return d.ID, nil
	}
	return "", errors.New("no free VFIO group available in the pool")
}

// filterPreferredFreeDevices returns available with busy devices moved to
// the end of the slice so kubelet's preferred-allocation pass picks free
// ones first when the available set is larger than the requested count.
// Returns the input slice when no busy devices are present, so the caller
// can detect the no-op case without an allocation.
func filterPreferredFreeDevices(available []string, bdfToIommu map[string]string) []string {
	hasBusy := false
	for _, id := range available {
		if iommu, ok := bdfToIommu[id]; ok && isVfioGroupBusy(iommu) {
			hasBusy = true
			break
		}
	}
	if !hasBusy {
		return available
	}
	free := make([]string, 0, len(available))
	busy := make([]string, 0)
	for _, id := range available {
		iommu, ok := bdfToIommu[id]
		if ok && isVfioGroupBusy(iommu) {
			busy = append(busy, id)
		} else {
			free = append(free, id)
		}
	}
	return slices.Concat(free, busy)
}
