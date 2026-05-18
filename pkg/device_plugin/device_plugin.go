/*
 * Copyright (c) 2019-2023, NVIDIA CORPORATION. All rights reserved.
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
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	klog "k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	nvidiaVendorID = "10de"
)

// Structure to hold details about Nvidia GPU Device
type NvidiaGpuDevice struct {
	addr     string // PCI address of device
	numaNode int64  // NUMA node ID
}

// mapsMu guards iommuMap, deviceMap, and bdfToIommuMap so that incremental
// additions from the vfio-watcher (after the initial discovery walk) do not
// race with reads from the device-plugin's Allocate() / health-check loops.
var mapsMu sync.RWMutex

// Key is iommu group id and value is a list of gpu devices part of the iommu group
var iommuMap map[string][]NvidiaGpuDevice

// Keys are the distinct Nvidia GPU device ids present on system and value is the list of all Nvidia GPU devices of that type
var deviceMap map[string][]NvidiaGpuDevice

// Maps PCI BDF to iommu group ids
var bdfToIommuMap map[string]string

// pluginRegistry maps a kubelet device-id (e.g. "2684" for an RTX 4090) to
// the live GenericDevicePlugin advertising it, so the vfio-watcher can push
// newly-bound GPUs into the right plugin's ListAndWatch stream without
// restarting the process. Guarded by pluginRegistryMu.
var (
	pluginRegistryMu sync.Mutex
	pluginRegistry   = map[string]*GenericDevicePlugin{}
)

// Key is vGPU Type and value is the list of Nvidia vGPUs of that type
var vGpuMap map[string][]NvidiaGpuDevice

// Key is the Nvidia GPU id and value is the list of associated vGPU ids
var gpuVgpuMap map[string][]string

var basePath = "/sys/bus/pci/devices"

// rootPath can be set for testing to simplify testing
var rootPath = "/"
var vGpuBasePath = "/sys/bus/mdev/devices"
var supportedVfioDrivers = map[string]struct{}{
	"vfio-pci":             {},
	"nvgrace_gpu_vfio_pci": {},
}
var pciIdsFilePath = "/usr/pci.ids"
var readLink = readLinkFunc
var readIDFromFile = readIDFromFileFunc
var readNUMANode = readNUMANodeFunc
var startDevicePlugin = startDevicePluginFunc
var readVgpuIDFromFile = readVgpuIDFromFileFunc
var readGpuIDForVgpu = readGpuIDForVgpuFunc
var startVgpuDevicePlugin = startVgpuDevicePluginFunc
var stop = make(chan struct{})

func InitiateDevicePlugin() {
	// Initial sysfs discovery of every NVIDIA GPU already bound to a
	// supported VFIO driver.
	createIommuDeviceMap()
	createVgpuIDMap()
	// Stream subsequent vfio-pci bindings into the live ListAndWatch frame
	// of the matching GenericDevicePlugin so kubelet sees the new device
	// without us exiting the process.
	go watchVfioBindings(stop, onLateVfioBinding)
	createDevicePlugins()
}

// Starts gpu pass through and vGPU device plugin
func createDevicePlugins() {
	var devicePlugins []*GenericDevicePlugin
	var vGpuDevicePlugins []*GenericVGpuDevicePlugin
	var devs []*pluginapi.Device
	// Snapshot deviceMap under the read lock so concurrent additions from
	// the vfio-watcher do not race with this initial iteration.
	mapsMu.RLock()
	deviceMapSnapshot := make(map[string][]NvidiaGpuDevice, len(deviceMap))
	for k, v := range deviceMap {
		cp := make([]NvidiaGpuDevice, len(v))
		copy(cp, v)
		deviceMapSnapshot[k] = cp
	}
	log.Printf("Iommu Map %v", iommuMap)
	log.Printf("Device Map %v", deviceMap)
	mapsMu.RUnlock()
	log.Println("vGPU Map ", vGpuMap)
	log.Println("GPU vGPU Map ", gpuVgpuMap)

	//Iterate over deivceMap to create device plugin for each type of GPU on the host
	for k, gpuDevices := range deviceMapSnapshot {
		devs = nil
		for _, gpuDev := range gpuDevices {
			device := &pluginapi.Device{
				ID:     gpuDev.addr,
				Health: pluginapi.Healthy,
				Topology: &pluginapi.TopologyInfo{
					Nodes: []*pluginapi.NUMANode{
						{ID: gpuDev.numaNode},
					},
				},
			}
			log.Printf("Registering device: ID=%s, NUMA=%d, Health=%s", device.ID, gpuDev.numaNode, device.Health)
			devs = append(devs, device)
		}
		deviceName := getDeviceName(k)
		if deviceName == "" {
			log.Printf("Error: Could not find device name for device id: %s", k)
			deviceName = k
		}
		log.Printf("DP Name %s", deviceName)
		dp := NewGenericDevicePlugin(deviceName, "/dev/vfio/", devs)
		// Register before Start so the vfio-watcher callback can already
		// reach this plugin if a late binding arrives during startup.
		registerPlugin(k, dp)
		err := startDevicePlugin(dp)
		if err != nil {
			log.Printf("Error starting %s device plugin: %v", dp.deviceName, err)
		} else {
			devicePlugins = append(devicePlugins, dp)
		}
	}
	//Iterate over vGpuMap to create device plugin for each type of vGPU on the host
	for k, v := range vGpuMap {
		devs = nil
		for _, dev := range v {
			devs = append(devs, &pluginapi.Device{
				ID:     dev.addr,
				Health: pluginapi.Healthy,
				Topology: &pluginapi.TopologyInfo{
					Nodes: []*pluginapi.NUMANode{
						{ID: dev.numaNode},
					},
				},
			})
		}
		deviceName := getDeviceName(k)
		if deviceName == "" {
			deviceName = k
		}
		log.Printf("DP Name %s", deviceName)
		dp := NewGenericVGpuDevicePlugin(deviceName, vGpuBasePath, devs)
		err := startVgpuDevicePlugin(dp)
		if err != nil {
			log.Printf("Error starting %s device plugin: %v", dp.deviceName, err)
		} else {
			vGpuDevicePlugins = append(vGpuDevicePlugins, dp)
		}
	}

	<-stop
	log.Printf("Shutting down device plugin controller")
	for _, v := range devicePlugins {
		v.Stop()
	}

	for _, v := range vGpuDevicePlugins {
		v.Stop()
	}

}

func startDevicePluginFunc(dp *GenericDevicePlugin) error {
	return dp.Start(stop)
}

func startVgpuDevicePluginFunc(dp *GenericVGpuDevicePlugin) error {
	return dp.Start(stop)
}

// Discovers all Nvidia GPUs which are loaded with VFIO-PCI driver and creates corresponding maps
func createIommuDeviceMap() {
	mapsMu.Lock()
	iommuMap = make(map[string][]NvidiaGpuDevice)
	deviceMap = make(map[string][]NvidiaGpuDevice)
	bdfToIommuMap = make(map[string]string)
	mapsMu.Unlock()
	//Walk directory to discover pci devices
	filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error accessing file path %q: %v\n", path, err)
			return err
		}
		if info.IsDir() {
			log.Println("Not a device, continuing")
			return nil
		}
		registerVfioBdf(info.Name())
		return nil
	})
}

// vfioDeviceInfo is the resolved sysfs view of a single NVIDIA PCI device
// bound to a supported VFIO driver, used to bridge the pure inspection step
// (inspectVfioPciDevice) and the map mutation step (recordVfioDevice).
type vfioDeviceInfo struct {
	bdf        string
	deviceID   string
	iommuGroup string
	gpu        NvidiaGpuDevice
}

// inspectVfioPciDevice reads the sysfs entries for a single PCI BDF and
// returns the resolved vfioDeviceInfo, or (nil, false) if the device is not
// an NVIDIA GPU bound to a supported VFIO driver. Pure read; no map mutation.
func inspectVfioPciDevice(bdf string) (*vfioDeviceInfo, bool) {
	vendorID, err := readIDFromFile(basePath, bdf, "vendor")
	if err != nil || vendorID != nvidiaVendorID {
		return nil, false
	}
	driver, err := readLink(basePath, bdf, "driver")
	if err != nil {
		log.Printf("inspectVfioPciDevice %s: read driver: %v", bdf, err)
		return nil, false
	}
	if !isSupportedVfioDriver(driver) {
		return nil, false
	}
	iommuGroup, err := readLink(basePath, bdf, "iommu_group")
	if err != nil {
		log.Printf("inspectVfioPciDevice %s: read iommu_group: %v", bdf, err)
		return nil, false
	}
	deviceID, err := readIDFromFile(basePath, bdf, "device")
	if err != nil {
		log.Printf("inspectVfioPciDevice %s: read device id: %v", bdf, err)
		return nil, false
	}
	numaNode, err := readNUMANode(basePath, bdf)
	if err != nil {
		log.Printf("inspectVfioPciDevice %s: read NUMA node: %v — defaulting to 0", bdf, err)
		numaNode = 0
	}
	return &vfioDeviceInfo{
		bdf:        bdf,
		deviceID:   deviceID,
		iommuGroup: iommuGroup,
		gpu:        NvidiaGpuDevice{addr: bdf, numaNode: numaNode},
	}, true
}

// recordVfioDevice atomically adds info to iommuMap / deviceMap / bdfToIommuMap
// under mapsMu. Returns false if the BDF was already known (so duplicate
// late-binding events are idempotent).
func recordVfioDevice(info *vfioDeviceInfo) bool {
	mapsMu.Lock()
	defer mapsMu.Unlock()
	if _, exists := bdfToIommuMap[info.bdf]; exists {
		return false
	}
	deviceMap[info.deviceID] = append(deviceMap[info.deviceID], info.gpu)
	iommuMap[info.iommuGroup] = append(iommuMap[info.iommuGroup], info.gpu)
	bdfToIommuMap[info.bdf] = info.iommuGroup
	return true
}

// registerVfioBdf is the discovery-time entry point used both by the
// initial sysfs walk and by the late-binding watcher. It performs the
// inspect-then-record pair and returns the resolved info so the caller
// can decide what to do with it (e.g. push into a live ListAndWatch frame).
// Returns (nil, false) if the BDF is not an NVIDIA VFIO GPU or is already
// known.
func registerVfioBdf(bdf string) (*vfioDeviceInfo, bool) {
	info, ok := inspectVfioPciDevice(bdf)
	if !ok {
		return nil, false
	}
	if !recordVfioDevice(info) {
		return nil, false
	}
	log.Printf("registerVfioBdf %s: added (device=%s, iommu_group=%s, numa=%d)", info.bdf, info.deviceID, info.iommuGroup, info.gpu.numaNode)
	return info, true
}

// registerPlugin associates a kubelet device id (e.g. "2684" for an RTX
// 4090) with the live GenericDevicePlugin advertising it, so
// onLateVfioBinding can push newly-bound BDFs into the right plugin's
// ListAndWatch stream.
func registerPlugin(deviceID string, dp *GenericDevicePlugin) {
	pluginRegistryMu.Lock()
	defer pluginRegistryMu.Unlock()
	pluginRegistry[deviceID] = dp
}

// onLateVfioBinding is invoked by watchVfioBindings for each new NVIDIA
// GPU bound to a VFIO driver after the initial discovery. It records the
// device and, if a plugin for this device id is already running, pushes
// the new device into its ListAndWatch stream.
//
// If no plugin exists yet for this device id — e.g. the first GPU of a
// previously-absent model is hot-plugged — the device is still recorded
// so it will be picked up on the next daemonset restart, but no live
// update can be sent until createDevicePlugins is re-run.
func onLateVfioBinding(bdf string) {
	info, ok := registerVfioBdf(bdf)
	if !ok {
		return
	}
	pluginRegistryMu.Lock()
	dp, hasPlugin := pluginRegistry[info.deviceID]
	pluginRegistryMu.Unlock()
	if !hasPlugin {
		log.Printf("onLateVfioBinding %s: no live plugin for device id %s yet — will be advertised on next daemon restart", bdf, info.deviceID)
		return
	}
	dp.AddDevice(&pluginapi.Device{
		ID:     info.gpu.addr,
		Health: pluginapi.Healthy,
		Topology: &pluginapi.TopologyInfo{
			Nodes: []*pluginapi.NUMANode{{ID: info.gpu.numaNode}},
		},
	})
}

func isSupportedVfioDriver(driver string) bool {
	_, exists := supportedVfioDrivers[driver]
	return exists
}

// Discovers all Nvidia vGPUs configured on a node and creates corresponding maps
func createVgpuIDMap() {
	vGpuMap = make(map[string][]NvidiaGpuDevice)
	gpuVgpuMap = make(map[string][]string)
	//Walk directory to discover vGPU devices
	filepath.Walk(vGpuBasePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error accessing file path %q: %v\n", path, err)
			return err
		}
		if info.IsDir() {
			log.Println("Not a device, continuing")
			return nil
		}
		//Read vGPU type name
		vGpuID, err := readVgpuIDFromFile(vGpuBasePath, info.Name(), "mdev_type/name")
		if err != nil {
			log.Println("Could not get vGPU type identifier for device ", info.Name())
			return nil
		}
		//Retrieve the gpu ID for this vGPU
		gpuID, err := readGpuIDForVgpu(vGpuBasePath, info.Name())
		if err != nil {
			log.Println("Could not get vGPU type identifier for device ", info.Name())
			return nil
		}
		numaNode, err := readNUMANode(basePath, gpuID)
		if err != nil {
			log.Printf("Could not get NUMA node for GPU %s: %v. Defaulting to NUMA node 0", gpuID, err)
			numaNode = 0
		}
		log.Printf("Gpu id is %s", gpuID)
		log.Printf("Vgpu id is %s", vGpuID)
		gpuVgpuMap[gpuID] = append(gpuVgpuMap[gpuID], info.Name())
		vGpuMap[vGpuID] = append(vGpuMap[vGpuID], NvidiaGpuDevice{addr: info.Name(), numaNode: numaNode})
		return nil
	})
}

// Read a file to retrieve ID
func readIDFromFileFunc(basePath string, deviceAddress string, property string) (string, error) {
	data, err := os.ReadFile(filepath.Join(basePath, deviceAddress, property))
	if err != nil {
		klog.Errorf("Could not read %s for device %s: %s", property, deviceAddress, err)
		return "", err
	}
	id := strings.Trim(string(data[2:]), "\n")
	return id, nil
}

func readNUMANodeFunc(basePath string, deviceAddress string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(basePath, deviceAddress, "numa_node"))
	if err != nil {
		klog.Errorf("Could not read NUMA node for device %s: %s", deviceAddress, err)
		return 0, err
	}
	nodeStr := strings.TrimSpace(string(data))
	nodeID, err := strconv.ParseInt(nodeStr, 10, 64)
	if err != nil {
		klog.Errorf("Could not parse NUMA node for device %s: %s", deviceAddress, err)
		return 0, err
	}
	if nodeID < 0 {
		return 0, nil
	}
	return nodeID, nil
}

// Read a file link
func readLinkFunc(basePath string, deviceAddress string, link string) (string, error) {
	path, err := os.Readlink(filepath.Join(basePath, deviceAddress, link))
	if err != nil {
		klog.Errorf("Could not read link %s for device %s: %s", link, deviceAddress, err)
		return "", err
	}
	_, file := filepath.Split(path)
	return file, nil
}

// Read vGPU type name from the corresponding file
func readVgpuIDFromFileFunc(basePath string, deviceAddress string, property string) (string, error) {
	reg := regexp.MustCompile("\\s+")
	data, err := os.ReadFile(filepath.Join(basePath, deviceAddress, property))
	if err != nil {
		klog.Errorf("Could not read %s for device %s: %s", property, deviceAddress, err)
		return "", err
	}
	str := strings.Trim(string(data[:]), "\n")
	str = reg.ReplaceAllString(str, "_") // Replace all spaces with underscore
	return str, nil
}

// Read GPU id for a specific vGPU
func readGpuIDForVgpuFunc(basePath string, deviceAddress string) (string, error) {
	path, err := os.Readlink(filepath.Join(basePath, deviceAddress))
	if err != nil {
		klog.Errorf("Could not read link for device %s: %s", deviceAddress, err)
		return "", err
	}
	splitStr := strings.Split(path, "/")
	length := len(splitStr)
	return strings.Trim(splitStr[length-2], "\n"), nil

}

// getIommuMap returns a snapshot copy of iommuMap taken under the maps
// read lock, so callers can iterate without racing concurrent additions
// from the vfio-watcher's onLateVfioBinding path.
func getIommuMap() map[string][]NvidiaGpuDevice {
	mapsMu.RLock()
	defer mapsMu.RUnlock()
	out := make(map[string][]NvidiaGpuDevice, len(iommuMap))
	for k, v := range iommuMap {
		cp := make([]NvidiaGpuDevice, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// getBdfToIommuMap returns a snapshot copy of bdfToIommuMap taken under
// the maps read lock.
func getBdfToIommuMap() map[string]string {
	mapsMu.RLock()
	defer mapsMu.RUnlock()
	out := make(map[string]string, len(bdfToIommuMap))
	for k, v := range bdfToIommuMap {
		out[k] = v
	}
	return out
}

func getGpuVgpuMap() map[string][]string {
	return gpuVgpuMap
}

func getDeviceName(deviceID string) string {
	deviceName := ""
	file, err := os.Open(pciIdsFilePath)
	if err != nil {
		log.Printf("Error opening pci ids file %s", pciIdsFilePath)
		return ""
	}
	defer file.Close()

	// Locate beginning of NVIDIA device list in pci.ids file
	scanner, err := locateVendor(file, nvidiaVendorID)
	if err != nil {
		log.Printf("Error locating NVIDIA in pci.ds file: %v", err)
		return ""
	}

	// Find NVIDIA device by device id
	prefix := fmt.Sprintf("\t%s", deviceID)
	for scanner.Scan() {
		line := scanner.Text()
		// ignore comments
		if strings.HasPrefix(line, "#") {
			continue
		}
		// if line does not start with tab, we are visiting a different vendor
		if !strings.HasPrefix(line, "\t") {
			log.Printf("Could not find NVIDIA device with id: %s", deviceID)
			return ""
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		deviceName = strings.TrimPrefix(line, prefix)
		deviceName = strings.TrimSpace(deviceName)
		deviceName = strings.ToUpper(deviceName)
		deviceName = strings.Replace(deviceName, "/", "_", -1)
		deviceName = strings.Replace(deviceName, ".", "_", -1)
		// Replace all spaces with underscore
		reg, _ := regexp.Compile("\\s+")
		deviceName = reg.ReplaceAllString(deviceName, "_")
		// Removes any char other than alphanumeric and underscore
		reg, _ = regexp.Compile("[^a-zA-Z0-9_.]+")
		deviceName = reg.ReplaceAllString(deviceName, "")
		break
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading pci ids file %s", err)
	}
	return deviceName
}

func locateVendor(pciIdsFile *os.File, vendorID string) (*bufio.Scanner, error) {
	scanner := bufio.NewScanner(pciIdsFile)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, vendorID) {
			return scanner, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return scanner, fmt.Errorf("error reading pci.ids file: %v", err)
	}

	return scanner, fmt.Errorf("failed to find vendor id in pci.ids file: %s", vendorID)
}
