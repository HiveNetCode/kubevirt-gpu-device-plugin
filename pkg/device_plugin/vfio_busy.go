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
	"path/filepath"

	"golang.org/x/sys/unix"
)

// isVfioGroupBusy reports whether /dev/vfio/<group> is currently held by
// another process (typically a running qemu-kvm / virt-launcher with a GPU
// passed through). The VFIO kernel driver returns EBUSY on the second
// open(2) of a group whose `opened` refcount is already 1, so a single
// non-blocking open is enough to distinguish "free" from "in use".
//
// Returning true tells the device plugin to exclude this device from the
// pool it advertises to kubelet, so a new pod is never assigned a PCI
// device that is already passed through to another tenant VM — preventing
// the "Could not open '/dev/vfio/<group>': Device or resource busy" qemu
// crashloop.
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
