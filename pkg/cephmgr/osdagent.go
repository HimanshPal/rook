/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cephmgr

import (
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	ctx "golang.org/x/net/context"

	etcd "github.com/coreos/etcd/client"
	"github.com/google/uuid"

	"github.com/rook/rook/pkg/cephmgr/client"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/util"
	"github.com/rook/rook/pkg/util/proc"
)

const (
	osdAgentName    = "osd"
	deviceKey       = "device"
	dirKey          = "dir"
	unassignedOSDID = -1
)

type osdAgent struct {
	cluster       *ClusterInfo
	forceFormat   bool
	location      string
	factory       client.ConnectionFactory
	osdProc       map[int]*proc.MonitoredProc
	devices       string
	configCounter int32
	osdsCompleted chan struct{}
}

type osdInfo struct {
	id     int
	serial string
	dir    bool
}

type osdConfig struct {
	deviceName string
	rootPath   string
	id         int
	uuid       uuid.UUID
	diskUUID   string
	bluestore  bool
}

func newOSDAgent(factory client.ConnectionFactory, devices string, forceFormat bool, location string) *osdAgent {
	a := &osdAgent{factory: factory, devices: devices, forceFormat: forceFormat, location: location}
	return a
}

func (a *osdAgent) Name() string {
	return osdAgentName
}

// set the desired state in etcd
func (a *osdAgent) Initialize(context *clusterd.Context) error {

	if len(a.devices) > 0 {
		// add the devices to desired state
		devices := strings.Split(a.devices, ",")
		for _, device := range devices {
			log.Printf("Adding device %s to desired state", device)
			err := AddDesiredDevice(context.EtcdClient, device, context.NodeID)
			if err != nil {
				return fmt.Errorf("failed to add desired device %s. %v", device, err)
			}
		}
	}

	// if no devices or directories were specified, use the current directory for an osd
	if len(a.devices) == 0 {
		log.Printf("Adding local path to local directory %s", context.ConfigDir)
		err := AddDesiredDir(context.EtcdClient, context.ConfigDir, context.NodeID)
		if err != nil {
			return fmt.Errorf("failed to add current dir %s. %v", context.ConfigDir, err)
		}
	}

	return nil
}

func (a *osdAgent) ConfigureLocalService(context *clusterd.Context) error {
	required, err := a.osdConfigRequired(context)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}

	// check if osd configuration is already in progress from a previous request
	if !a.tryStartConfig() {
		return nil
	}

	defer a.decrementConfigCounter()

	a.cluster, err = LoadClusterInfo(context.EtcdClient)
	if err != nil {
		return fmt.Errorf("failed to load cluster info: %v", err)
	}
	if a.cluster == nil {
		// the ceph cluster is not initialized yet
		return nil
	}

	// Connect to the ceph cluster
	adminConn, err := ConnectToClusterAsAdmin(context, a.factory, a.cluster)
	if err != nil {
		return err
	}
	defer adminConn.Shutdown()

	if err := a.createDesiredOSDs(adminConn, context); err != nil {
		return err
	}

	return a.stopUndesiredDevices(context, adminConn)
}

// check if osd configured is required at this time
// 1) the node should be marked in the desired state
// 2) osd configuration must not already be in progress from a previous orchestration
func (a *osdAgent) osdConfigRequired(context *clusterd.Context) (bool, error) {
	key := path.Join(cephKey, osdAgentName, desiredKey, context.NodeID, "ready")
	osdsDesired, err := context.EtcdClient.Get(ctx.Background(), key, nil)
	if err != nil {
		if util.IsEtcdKeyNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get osd desired state. %v", err)
	}

	if osdsDesired.Node.Value != "1" {
		// The osd is not in desired state
		return false, nil
	}

	return true, nil
}

// Try to enter the critical section for configuring osds.
// If a configuration is already in progress, returns false.
// If configuration can be started, returns true.
// The caller of this method must call decrementConfigCounter() if true is returned.
func (a *osdAgent) tryStartConfig() bool {
	counter := atomic.AddInt32(&a.configCounter, 1)
	if counter > 1 {
		counter = atomic.AddInt32(&a.configCounter, -1)
		log.Printf("osd configuration is already running. counter=%d", counter)
		return false
	}

	return true
}

// increment the config counter when a config step starts
func (a *osdAgent) incrementConfigCounter() {
	atomic.AddInt32(&a.configCounter, 1)
}

// decrement the config counter when a config step is completed.
func (a *osdAgent) decrementConfigCounter() {
	atomic.AddInt32(&a.configCounter, -1)
}

func (a *osdAgent) stopUndesiredDevices(context *clusterd.Context, connection client.Connection) error {
	desiredDevices, err := loadDesiredDevices(context.EtcdClient, context.NodeID)
	if err != nil {
		return fmt.Errorf("failed to load desired devices. %v", err)
	}

	desiredDirs, err := loadDesiredDirs(context.EtcdClient, context.NodeID)
	if err != nil {
		return fmt.Errorf("failed to load desired dirs. %v", err)
	}

	applied, err := GetAppliedOSDs(context.NodeID, context.EtcdClient)
	if err != nil {
		return fmt.Errorf("failed to get applied OSDs. %v", err)
	}

	desiredOSDs := map[int]interface{}{}
	for _, id := range desiredDevices {
		desiredOSDs[id] = nil
	}
	for _, id := range desiredDirs {
		desiredOSDs[id] = nil
	}

	log.Printf("stopUndesiredDevices. applied=%+v, desired=%+v", applied, desiredOSDs)
	var lastErr error
	for appliedOSD := range applied {
		if _, ok := desiredOSDs[appliedOSD]; ok {
			// the osd is both desired and applied
			continue
		}

		log.Printf("removing osd %d", appliedOSD)
		err := a.removeOSD(context, connection, appliedOSD)
		if err != nil {
			lastErr = err
		}
	}

	return lastErr
}

func (a *osdAgent) removeOSD(context *clusterd.Context, connection client.Connection, id int) error {

	// mark the OSD as out of the cluster so its data starts to migrate
	err := markOSDOut(connection, id)
	if err != nil {
		return fmt.Errorf("failed to mark out osd %d. %v", id, err)
	}

	// stop the osd process if running
	proc, ok := a.osdProc[id]
	if ok {
		err := proc.Stop()
		if err != nil {
			log.Printf("failed to stop osd %d. %v", id, err)
			return err
		}

		delete(a.osdProc, id)
	}

	err = purgeOSD(connection, id)
	if err != nil {
		return fmt.Errorf("faild to remove osd %d from crush map. %v", id, err)
	}

	// remove the osd from the applied key
	appliedKey := path.Join(getAppliedKey(context.NodeID), fmt.Sprintf("%d", id))
	_, err = context.EtcdClient.Delete(ctx.Background(), appliedKey, &etcd.DeleteOptions{Recursive: true, Dir: true})
	if err != nil {
		log.Printf("failed to remove osd %d from applied state. %v", id, err)
		return err
	}

	log.Printf("Stopped and removed osd device %d", id)

	return nil
}

func (a *osdAgent) DestroyLocalService(context *clusterd.Context) error {
	// stop the OSD processes
	for id, proc := range a.osdProc {
		log.Printf("stopping osd %d", id)
		proc.Stop()
	}

	// clear out the osd procs
	a.osdProc = map[int]*proc.MonitoredProc{}
	return nil
}

func getAppliedKey(nodeID string) string {
	return path.Join(cephKey, osdAgentName, appliedKey, nodeID)
}

// create and initalize OSDs for all the devices specified in the given config
func (a *osdAgent) createDesiredOSDs(adminConn client.Connection, context *clusterd.Context) error {
	devices, err := loadDesiredDevices(context.EtcdClient, context.NodeID)
	if err != nil {
		return fmt.Errorf("failed to load desired devices. %v", err)
	}

	dirs, err := loadDesiredDirs(context.EtcdClient, context.NodeID)
	if err != nil {
		return fmt.Errorf("failed to load desired dirs. %v", err)
	}

	// generate and write the OSD bootstrap keyring
	if err := createOSDBootstrapKeyring(adminConn, context.ConfigDir, a.cluster.Name); err != nil {
		return err
	}

	// initialize the desired OSD directories
	err = a.configureDirs(context, dirs)
	if err != nil {
		return err
	}

	return a.configureDevices(context, devices)
}

func (a *osdAgent) configureDirs(context *clusterd.Context, dirs map[string]int) error {
	if len(dirs) == 0 {
		return nil
	}

	// connect to the cluster using the bootstrap-osd creds, this connection will be used for config operations
	connection, err := a.getBoostrapOSDConnection(context)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster to config filestore osds. %+v", err)
	}
	defer connection.Shutdown()

	succeeded := 0
	var lastErr error
	for dir, osdID := range dirs {
		config := &osdConfig{id: osdID}
		err := a.createOrStartOSD(context, connection, config, dir, true)
		if err != nil {
			log.Printf("ERROR: failed to config osd in path %s. %+v", dir, err)
			lastErr = err
		} else {
			succeeded++
		}
	}

	log.Printf("%d/%d osds (filestore) succeeded on this node", succeeded, len(dirs))
	return lastErr

}

func (a *osdAgent) getBoostrapOSDConnection(context *clusterd.Context) (client.Connection, error) {
	return connectToCluster(context, a.factory, a.cluster,
		getBootstrapOSDDir(context.ConfigDir), "bootstrap-osd",
		getBootstrapOSDKeyringPath(context.ConfigDir, a.cluster.Name), context.Debug)
}

func (a *osdAgent) configureDevices(context *clusterd.Context, devices map[string]int) error {
	if len(devices) == 0 {
		return nil
	}

	// reset the signal that the osd config is in progress
	a.osdsCompleted = make(chan struct{})

	// connect to the cluster using the bootstrap-osd creds, this connection will be used for config operations
	connection, err := a.getBoostrapOSDConnection(context)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster to config bluestore osds. %+v", err)
	}

	// asynchronously configure all of the devices with osds
	go func() {
		defer connection.Shutdown()

		a.incrementConfigCounter()
		defer a.decrementConfigCounter()

		// initialize all the desired OSD volumes
		succeeded := 0
		for device, osdID := range devices {
			config := &osdConfig{id: osdID, deviceName: device, bluestore: true}
			err := a.createOrStartOSD(context, connection, config, context.ConfigDir, false)
			if err != nil {
				log.Printf("ERROR: failed to config osd on device %s. %+v", device, err)
			} else {
				succeeded++
			}
		}

		log.Printf("%d/%d bluestore osds succeeded on this node", succeeded, len(devices))

		// set the signal that the osd config is completed
		close(a.osdsCompleted)
	}()

	return nil
}

func (a *osdAgent) createOrStartOSD(context *clusterd.Context, connection client.Connection, config *osdConfig, configRoot string, dir bool) error {
	// create a new OSD in ceph unless already done previously
	if config.id == unassignedOSDID {
		err := registerOSD(connection, config)
		if err != nil {
			return err
		}

		name := config.deviceName
		if dir {
			name = configRoot
		}
		err = setOSDOnDevice(context.EtcdClient, context.NodeID, name, config.id, dir)
		if err != nil {
			return err
		}
	}

	newOSD := false
	config.rootPath = path.Join(configRoot, fmt.Sprintf("osd%d", config.id))
	if isOSDDataNotExist(config.rootPath) {
		// consider this a new osd if the "whoami" file is not found
		newOSD = true

		// ensure the config path exists
		if err := os.MkdirAll(config.rootPath, 0744); err != nil {
			return fmt.Errorf("failed to make osd %d config at %s: %+v", config.id, config.rootPath, err)
		}
	}

	if newOSD {
		if config.bluestore {
			// the device needs to be formatted
			err := formatDevice(context, config, a.forceFormat)
			if err != nil {
				return fmt.Errorf("failed device %s. %+v", config.deviceName, err)
			}

			log.Printf("waiting after bluestore partition/format...")
			<-time.After(2 * time.Second)
		}

		// osd_data_dir/whoami does not exist yet, create/initialize the OSD
		err := initializeOSD(config, a.factory, context, connection, a.cluster, a.location, context.Debug, context.Executor)
		if err != nil {
			return fmt.Errorf("failed to initialize OSD at %s: %+v", config.rootPath, err)
		}

		// save the osd to applied state
		settings := map[string]string{
			"path":      configRoot,
			"disk-uuid": config.diskUUID,
		}
		key := path.Join(getAppliedKey(context.NodeID), fmt.Sprintf("%d", config.id))
		if err := util.StoreEtcdProperties(context.EtcdClient, key, settings); err != nil {
			return fmt.Errorf("failed to mark osd %d as applied: %+v", config.id, err)
		}

	} else {
		// osd_data_dir/whoami already exists, meaning the OSD is already set up.
		// look up some basic information about it so we can run it.
		err := loadOSDInfo(config)
		if err != nil {
			return fmt.Errorf("failed to get OSD information from %s: %+v", config.rootPath, err)
		}
	}

	// run the OSD in a child process now that it is fully initialized and ready to go
	err := a.runOSD(context, a.cluster.Name, config)
	if err != nil {
		return fmt.Errorf("failed to run osd %d: %+v", config.id, err)
	}

	return nil
}

// runs an OSD with the given config in a child process
func (a *osdAgent) runOSD(context *clusterd.Context, clusterName string, config *osdConfig) error {
	// start the OSD daemon in the foreground with the given config
	log.Printf("starting osd %d at %s", config.id, config.rootPath)

	osdUUIDArg := fmt.Sprintf("--osd-uuid=%s", config.uuid.String())

	params := []string{"--foreground",
		fmt.Sprintf("--id=%d", config.id),
		fmt.Sprintf("--cluster=%s", clusterName),
		fmt.Sprintf("--osd-data=%s", config.rootPath),
		fmt.Sprintf("--conf=%s", getOSDConfFilePath(config.rootPath, clusterName)),
		fmt.Sprintf("--keyring=%s", getOSDKeyringPath(config.rootPath)),
		osdUUIDArg,
	}

	if !config.bluestore {
		params = append(params, fmt.Sprintf("--osd-journal=%s", getOSDJournalPath(config.rootPath)))
	}

	process, err := context.ProcMan.Start(
		"osd",
		regexp.QuoteMeta(osdUUIDArg),
		proc.ReuseExisting,
		params...)
	if err != nil {
		return fmt.Errorf("failed to start osd %d: %+v", config.id, err)
	}

	if a.osdProc == nil {
		// initialize the osd map
		a.osdProc = make(map[int]*proc.MonitoredProc)
	}
	if process != nil {
		// if the process was already running Start will return nil in which case we don't want to overwrite it
		a.osdProc[config.id] = process
	}

	return nil
}

// For all applied OSDs, gets a mapping of their osd IDs to their device uuid
func GetAppliedOSDs(nodeID string, etcdClient etcd.KeysAPI) (map[int]string, error) {

	osds := map[int]string{}
	key := getAppliedKey(nodeID)
	osdKeys, err := etcdClient.Get(ctx.Background(), key, &etcd.GetOptions{Recursive: true})
	if err != nil {
		if util.IsEtcdKeyNotFound(err) {
			return osds, nil
		}
		return nil, err
	}

	// parse the osds from etcd
	for _, idKey := range osdKeys.Node.Nodes {
		id, err := strconv.Atoi(util.GetLeafKeyPath(idKey.Key))
		if err != nil {
			// skip the unexpected osd id
			continue
		}

		for _, setting := range idKey.Nodes {
			if strings.HasSuffix(setting.Key, "/disk-uuid") {
				osds[id] = setting.Value
			}
		}
	}

	return osds, nil
}

func getPseudoDir(path string) string {
	// cut off the leading slash so we won't end up with a hidden etcd key
	if strings.HasPrefix(path, "/") {
		path = path[1:]
	}

	return strings.Replace(path, "/", "_", -1)
}
