/*
 * Copyright (c) 2026, HiveNet. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package device_plugin

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

const pciDriversBasePath = "/sys/bus/pci/drivers"

var stopOnce sync.Once

// watchVfioBindings detects NVIDIA GPUs bound to a supported VFIO driver
// after this process completed its initial discovery, and triggers an orderly
// shutdown so the kubelet recreates the pod and re-runs discovery.
//
// This closes a startup race with nvidia-vfio-manager: createIommuDeviceMap
// runs once in InitiateDevicePlugin, so any GPU bound to a VFIO driver
// afterwards is invisible to kubelet — and therefore unschedulable — until the
// plugin pod is restarted by hand or by an external watchdog.
func watchVfioBindings(stop chan struct{}) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("vfio-watcher: cannot create fsnotify watcher: %v", err)
		return
	}
	defer watcher.Close()

	baseline := nvidiaVfioBdfs()
	watchedDrivers := make([]string, 0, len(supportedVfioDrivers))
	for driver := range supportedVfioDrivers {
		path := filepath.Join(pciDriversBasePath, driver)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := watcher.Add(path); err != nil {
			log.Printf("vfio-watcher: cannot watch %s: %v", path, err)
			continue
		}
		watchedDrivers = append(watchedDrivers, driver)
	}
	if len(watchedDrivers) == 0 {
		log.Printf("vfio-watcher: no supported VFIO driver directories present, exiting")
		return
	}
	log.Printf("vfio-watcher: watching drivers=%v; baseline=%d NVIDIA GPU(s)", watchedDrivers, len(baseline))

	// Re-check after Add to close the race window between baseline capture
	// and the watcher becoming active.
	if extra := newNvidiaBdfs(baseline); len(extra) > 0 {
		triggerRestart(stop, "post-baseline rescan found new NVIDIA GPU(s): "+strings.Join(extra, ","))
		return
	}

	for {
		select {
		case <-stop:
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create == 0 {
				continue
			}
			bdf := filepath.Base(ev.Name)
			if !isPCIAddress(bdf) {
				continue
			}
			if _, seen := baseline[bdf]; seen {
				continue
			}
			vendor, err := readIDFromFile(basePath, bdf, "vendor")
			if err != nil || vendor != nvidiaVendorID {
				continue
			}
			triggerRestart(stop, "new NVIDIA GPU "+bdf+" bound to a VFIO driver")
			return
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("vfio-watcher: error: %v", err)
		}
	}
}

// nvidiaVfioBdfs returns the set of NVIDIA PCI addresses currently bound to
// any supported VFIO driver.
func nvidiaVfioBdfs() map[string]struct{} {
	s := make(map[string]struct{})
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
				s[bdf] = struct{}{}
			}
		}
	}
	return s
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

// isPCIAddress reports whether s looks like a PCI BDF (e.g. "0000:01:00.0").
// This filters out non-device entries the kernel exposes under driver dirs
// (e.g. "bind", "unbind", "new_id", "module").
func isPCIAddress(s string) bool {
	return strings.HasPrefix(s, "0000:") && strings.ContainsRune(s, '.')
}

func triggerRestart(stop chan struct{}, reason string) {
	log.Printf("vfio-watcher: triggering plugin restart: %s", reason)
	stopOnce.Do(func() {
		close(stop)
	})
}
