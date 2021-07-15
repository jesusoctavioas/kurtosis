/* * Copyright (c) 2020 - present Kurtosis Technologies LLC.
 * All Rights Reserved.
 */

package service_network

import (
	"bytes"
	"context"
	"github.com/docker/go-connections/nat"
	"github.com/kurtosis-tech/kurtosis-client/golang/core_api_bindings"
	"github.com/kurtosis-tech/kurtosis/api_container/server/service_network/networking_sidecar"
	"github.com/kurtosis-tech/kurtosis/api_container/server/service_network/partition_topology"
	"github.com/kurtosis-tech/kurtosis/api_container/server/service_network/service_network_types"
	"github.com/kurtosis-tech/kurtosis/api_container/server/service_network/user_service_launcher"
	"github.com/kurtosis-tech/kurtosis/commons"
	"github.com/kurtosis-tech/kurtosis/commons/docker_manager"
	"github.com/kurtosis-tech/kurtosis/commons/suite_execution_volume"
	"github.com/palantir/stacktrace"
	"github.com/sirupsen/logrus"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultPartitionId                   service_network_types.PartitionID = "default"
	startingDefaultConnectionBlockStatus                                   = false
)

// Information that gets created with a service's registration
type serviceRegistrationInfo struct {
	ipAddr net.IP
	serviceDirectory *suite_execution_volume.ServiceDirectory
}

// Information that gets created when a container is started for a service
type serviceRunInfo struct {
	// Service's container ID
	containerId string

	// Where the suite execution volume is mounted on the service
	suiteExecutionVolumeMountDirpath string
}

/*
This is the in-memory representation of the service network that the API container will manipulate. To make
	any changes to the test network, this struct must be used.
 */
type ServiceNetworkImpl struct {
	// When the network is destroyed, all requests will fail
	// This ensures that when the initializer tells the API container to destroy everything, the still-running
	//  testsuite can't create more work
	isDestroyed bool   	// VERY IMPORTANT TO CHECK AT THE START OF EVERY METHOD!

	mutex *sync.Mutex	// VERY IMPORTANT TO CHECK AT THE START OF EVERY METHOD!

	// Whether partitioning has been enabled for this particular test
	isPartitioningEnabled bool

	freeIpAddrTracker *commons.FreeIpAddrTracker

	dockerManager *docker_manager.DockerManager

	testExecutionDirectory *suite_execution_volume.EnclaveDirectory

	staticFileCache *suite_execution_volume.StaticFileCache

	userServiceLauncher *user_service_launcher.UserServiceLauncher

	topology *partition_topology.PartitionTopology

	// These are separate maps, rather than being bundled into a single containerInfo-valued map, because
	//  they're registered at different times (rather than in one atomic operation)
	serviceRegistrationInfo map[service_network_types.ServiceID]serviceRegistrationInfo
	serviceRunInfo map[service_network_types.ServiceID]serviceRunInfo

	networkingSidecars map[service_network_types.ServiceID]networking_sidecar.NetworkingSidecar

	networkingSidecarManager networking_sidecar.NetworkingSidecarManager
}

func NewServiceNetworkImpl(
		isPartitioningEnabled bool,
		freeIpAddrTracker *commons.FreeIpAddrTracker,
		dockerManager *docker_manager.DockerManager,
		testExecutionDirectory *suite_execution_volume.EnclaveDirectory,
		staticFileCache *suite_execution_volume.StaticFileCache,
		userServiceLauncher *user_service_launcher.UserServiceLauncher,
		networkingSidecarManager networking_sidecar.NetworkingSidecarManager) *ServiceNetworkImpl {
	defaultPartitionConnection := partition_topology.PartitionConnection{IsBlocked: startingDefaultConnectionBlockStatus}
	return &ServiceNetworkImpl{
		isDestroyed: false,
		isPartitioningEnabled: isPartitioningEnabled,
		freeIpAddrTracker: freeIpAddrTracker,
		dockerManager: dockerManager,
		testExecutionDirectory: testExecutionDirectory,
		staticFileCache: staticFileCache,
		userServiceLauncher: userServiceLauncher,
		mutex:               &sync.Mutex{},
		topology:            partition_topology.NewPartitionTopology(
			defaultPartitionId,
			defaultPartitionConnection,
		),
		serviceRegistrationInfo:  map[service_network_types.ServiceID]serviceRegistrationInfo{},
		serviceRunInfo:           map[service_network_types.ServiceID]serviceRunInfo{},
		networkingSidecars:       map[service_network_types.ServiceID]networking_sidecar.NetworkingSidecar{},
		networkingSidecarManager: networkingSidecarManager,
	}
}

/*
Completely repartitions the network, throwing away the old topology
 */
func (network *ServiceNetworkImpl) Repartition(
		ctx context.Context,
		newPartitionServices map[service_network_types.PartitionID]*service_network_types.ServiceIDSet,
		newPartitionConnections map[service_network_types.PartitionConnectionID]partition_topology.PartitionConnection,
		newDefaultConnection partition_topology.PartitionConnection) error {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return stacktrace.NewError("Cannot repartition; the service network has been destroyed")
	}

	if !network.isPartitioningEnabled {
		return stacktrace.NewError("Cannot repartition; partitioning is not enabled")
	}

	if err := network.topology.Repartition(newPartitionServices, newPartitionConnections, newDefaultConnection); err != nil {
		return stacktrace.Propagate(err, "An error occurred repartitioning the network topology")
	}
	blocklists, err := network.topology.GetBlocklists()
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the blocklists after repartition, meaning that " +
			"no partitions are actually being enforced!")
	}
	if err := updateIpTables(ctx, blocklists, network.serviceRegistrationInfo, network.networkingSidecars); err != nil {
		return stacktrace.Propagate(err, "An error occurred updating the IP tables to match the target blocklist after repartitioning")
	}
	return nil
}

// Registers a service for use with the network (creating the IPs and so forth), but doesn't start it
// If the partition ID is empty, registers the service with the default partition
func (network ServiceNetworkImpl) RegisterService(
		serviceId service_network_types.ServiceID,
		partitionId service_network_types.PartitionID) (net.IP, error) {
	// TODO extract this into a wrapper function that can be wrapped around every service call (so we don't forget)
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot register service with ID '%v'; the service network has been destroyed", serviceId)
	}

	if strings.TrimSpace(string(serviceId)) == "" {
		return nil, stacktrace.NewError("Service ID cannot be empty or whitespace")
	}

	if _, found := network.serviceRegistrationInfo[serviceId]; found {
		return nil, stacktrace.NewError("Cannot register service with ID '%v'; a service with that ID already exists", serviceId)
	}

	if partitionId == "" {
		partitionId = defaultPartitionId
	}
	if _, found := network.topology.GetPartitionServices()[partitionId]; !found {
		return nil, stacktrace.NewError(
			"No partition with ID '%v' exists in the current partition topology",
			partitionId,
		)
	}

	serviceDirectory, err := network.testExecutionDirectory.NewServiceDirectory(string(serviceId))
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred creating a new service directory for service with ID '%v'", serviceId)
	}
	ip, err := network.freeIpAddrTracker.GetFreeIpAddr()
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting an IP for service with ID '%v'", serviceId)
	}
	shouldFreeIpAddr := true
	defer func() {
		// To keep our bookkeeping correct, if an error occurs later we need to back out the IP-adding that we do now
		if shouldFreeIpAddr {
			network.freeIpAddrTracker.ReleaseIpAddr(ip)
		}
	}()
	logrus.Debugf("Giving service '%v' IP '%v'", serviceId, ip.String())

	registrationInfo := serviceRegistrationInfo{
		ipAddr:           ip,
		serviceDirectory: serviceDirectory,
	}
	network.serviceRegistrationInfo[serviceId] = registrationInfo
	shouldUndoRegistrationInfoAdd := true
	defer func() {
		// If an error occurs, the service ID won't be used so we need to delete it from the map
		if shouldUndoRegistrationInfoAdd {
			delete(network.serviceRegistrationInfo, serviceId)
		}
	}()

	if err := network.topology.AddService(serviceId, partitionId); err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred adding service with ID '%v' to partition '%v' in the topology",
			serviceId,
			partitionId)
	}

	shouldFreeIpAddr = false
	shouldUndoRegistrationInfoAdd = false
	return ip, nil
}

// Generates files in a location in the suite execution volume allocated to the given service
func (network *ServiceNetworkImpl) GenerateFiles(
		serviceId service_network_types.ServiceID,
		filesToGenerate map[string]*core_api_bindings.FileGenerationOptions) (map[string]string, error) {
	// TODO extract this into a wrapper function that can be wrapped around every service call (so we don't forget)
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot register service with ID '%v'; the service network has been destroyed", serviceId)
	}

	registrationInfo, found := network.serviceRegistrationInfo[serviceId]
	if !found {
		return nil, stacktrace.NewError("Cannot generate files for service '%v' as no service with that ID has been registered", serviceId)
	}
	serviceDirectory := registrationInfo.serviceDirectory

	generatedFilesRelativeFilepaths := map[string]string{}
	for userCreatedFileKey, fileGenerationOptions := range filesToGenerate {
		fileTypeToGenerate := fileGenerationOptions.GetFileTypeToGenerate()
		switch fileTypeToGenerate {
		case core_api_bindings.FileGenerationOptions_FILE:
			file, err := serviceDirectory.NewGeneratedFile(userCreatedFileKey)
			if err != nil {
				return nil, stacktrace.Propagate(err, "An error occurred creating file '%v' for service with ID '%v'", userCreatedFileKey, serviceId)
			}
			generatedFilesRelativeFilepaths[userCreatedFileKey] = file.GetFilepathRelativeToVolRoot()
			logrus.Debugf("Created generated file '%v' at '%v'", userCreatedFileKey, file.GetFilepathRelativeToVolRoot())
		default:
			return nil, stacktrace.NewError(
				"Could not generate file '%v'; unrecognized file type '%v'; this is a bug in Kurtosis",
				userCreatedFileKey,
				fileTypeToGenerate,
			)
		}
	}
	return generatedFilesRelativeFilepaths, nil
}

func (network *ServiceNetworkImpl) LoadStaticFiles(serviceId service_network_types.ServiceID, staticFileIdKeys map[string]bool) (map[string]string, error) {
	// TODO extract this into a wrapper function that can be wrapped around every service call (so we don't forget)
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot load static files for service with ID '%v'; the service network has been destroyed", serviceId)
	}

	registrationInfo, found := network.serviceRegistrationInfo[serviceId]
	if !found {
		return nil, stacktrace.NewError("Cannot load static files for service '%v' as no service with that ID has been registered", serviceId)
	}
	serviceDirectory := registrationInfo.serviceDirectory

	result := map[string]string{}
	for staticFileId := range staticFileIdKeys {
		src, err := network.staticFileCache.GetEntry(staticFileId)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting the source static file with key '%v' from the static file cache", staticFileId)
		}

		dest, err := serviceDirectory.NewStaticFile(staticFileId)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting the destination static file for key '%v' in filespace of service '%v'", staticFileId, serviceId)
		}

		srcAbsFilepath := src.GetAbsoluteFilepath()
		srcFp, err := os.Open(srcAbsFilepath)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting the file pointer from source static file filepath '%v'", srcAbsFilepath)
		}
		defer srcFp.Close()

		destAbsFilepath := dest.GetAbsoluteFilepath()
		destFp, err := os.Create(destAbsFilepath)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred creating the file pointer for destination static file filepath '%v'", destAbsFilepath)
		}
		defer destFp.Close()

		if _, err := io.Copy(destFp, srcFp); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred copying source static file '%v' to destination '%v'", srcAbsFilepath, destAbsFilepath)
		}

		result[staticFileId] = src.GetFilepathRelativeToVolRoot()
	}
	return result, nil
}

// TODO add tests for this
/*
Starts a previously-registered but not-started service by creating it in a container

Returns:
	Mapping of port-used-by-service -> port-on-the-Docker-host-machine where the user can make requests to the port
		to access the port. If a used port doesn't have a host port bound, then the value will be nil.
 */
func (network *ServiceNetworkImpl) StartService(
		ctx context.Context,
		serviceId service_network_types.ServiceID,
		imageName string,
		usedPorts map[nat.Port]bool,
		entrypointArgs []string,
		cmdArgs []string,
		dockerEnvVars map[string]string,
		suiteExecutionVolMntDirpath string,
		filesArtifactMountDirpaths map[string]string) (map[nat.Port]*nat.PortBinding, error) {
	// TODO extract this into a wrapper function that can be wrapped around every service call (so we don't forget)
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot start container for service with ID '%v'; the service network has been destroyed", serviceId)
	}

	registrationInfo, registrationInfoFound := network.serviceRegistrationInfo[serviceId]
	if !registrationInfoFound {
		return nil, stacktrace.NewError("Cannot start container for service with ID '%v'; no service with that ID has been registered", serviceId)
	}
	if _, found := network.serviceRunInfo[serviceId]; found {
		return nil, stacktrace.NewError("Cannot start container for service with ID '%v'; that service ID already has run information associated with it", serviceId)
	}

	// When partitioning is enabled, there's a race condition where:
	//   a) we need to start the service before we can launch the sidecar but
	//   b) we can't modify the iptables until the sidecar container is launched.
	// This means that there's a period of time at startup where the container might not be partitioned. We solve
	//  this by blocking the new service's IP in the already-existing services' iptables BEFORE we start the service.
	// This means that when the new service is launched, even if its own iptables aren't yet updated, all the services
	//  it would communicate are already dropping traffic from it.
	if network.isPartitioningEnabled {
		blocklists, err := network.topology.GetBlocklists()
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting the blocklists for updating iptables before the " +
				"node was added, which means we can't add the node because we can't partition it away properly")
		}
		blocklistsWithoutNewNode := map[service_network_types.ServiceID]*service_network_types.ServiceIDSet{}
		for serviceInTopologyId, servicesToBlock := range blocklists {
			if serviceId == serviceInTopologyId {
				continue
			}
			blocklistsWithoutNewNode[serviceInTopologyId] = servicesToBlock
		}
		if err := updateIpTables(ctx, blocklistsWithoutNewNode, network.serviceRegistrationInfo, network.networkingSidecars); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred updating the iptables of all the other services " +
				"before adding the node, meaning that the node wouldn't actually start in a partition")
		}
	}

	serviceContainerId, hostPortBindings, err := network.userServiceLauncher.Launch(
		ctx,
		serviceId,
		registrationInfo.ipAddr,
		imageName,
		usedPorts,
		entrypointArgs,
		cmdArgs,
		dockerEnvVars,
		suiteExecutionVolMntDirpath,
		filesArtifactMountDirpaths)
	if err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred creating the user service container")
	}
	runInfo := serviceRunInfo{
		containerId:                      serviceContainerId,
		suiteExecutionVolumeMountDirpath: suiteExecutionVolMntDirpath,
	}
	network.serviceRunInfo[serviceId] = runInfo

	if network.isPartitioningEnabled {
		sidecar, err := network.networkingSidecarManager.Create(ctx, serviceId, serviceContainerId)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred creating the networking sidecar container")
		}
		network.networkingSidecars[serviceId] = sidecar

		if err := sidecar.InitializeIpTables(ctx); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred initializing the newly-created sidecar container iptables")
		}

		// TODO Getting blocklists is an expensive call and, as of 2020-12-31, we do it twice - the solution is to make
		//  getting the blocklists not an expensive call (see also https://github.com/kurtosis-tech/kurtosis-core/issues/123 )
		blocklists, err := network.topology.GetBlocklists()
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting the blocklists to know what iptables " +
				"updates to apply on the new node")
		}
		newNodeBlocklist := blocklists[serviceId]
		updatesToApply := map[service_network_types.ServiceID]*service_network_types.ServiceIDSet{
			serviceId: newNodeBlocklist,
		}
		if err := updateIpTables(ctx, updatesToApply, network.serviceRegistrationInfo, network.networkingSidecars); err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred applying the iptables on the new node to partition it " +
				"off from other nodes")
		}
	}

	return hostPortBindings, nil
}

func (network *ServiceNetworkImpl) RemoveService(
		ctx context.Context,
		serviceId service_network_types.ServiceID,
		containerStopTimeout time.Duration) error {
	// TODO switch to a wrapper function
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return stacktrace.NewError("Cannot remove service; the service network has been destroyed")
	}

	if err := network.removeServiceWithoutMutex(ctx, serviceId, containerStopTimeout); err != nil {
		return stacktrace.Propagate(err, "An error occurred removing service with ID '%v'", serviceId)
	}
	return nil
}

func (network *ServiceNetworkImpl) ExecCommand(
		ctx context.Context,
		serviceId service_network_types.ServiceID,
		command []string) (int32, *bytes.Buffer, error) {
	// NOTE: This will block all other operations while this command is running!!!! We might need to change this so it's
	// asynchronous
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return 0, nil, stacktrace.NewError("Cannot run exec command; the service network has been destroyed")
	}

	runInfo, found := network.serviceRunInfo[serviceId]
	if !found {
		return 0, nil, stacktrace.NewError(
			"Could not run exec command '%v' against service '%v'; no container has been created for the service yet",
			command,
			serviceId)
	}

	// NOTE: This is a SYNCHRONOUS command, meaning that the entire network will be blocked until the command finishes
	// In the future, this will likely be insufficient

	execOutputBuf := &bytes.Buffer{}
	exitCode, err := network.dockerManager.RunExecCommand(ctx, runInfo.containerId, command, execOutputBuf)
	if err != nil {
		return 0, nil, stacktrace.Propagate(
			err,
			"An error occurred running exec command '%v' against service '%v'",
			command,
			serviceId)
	}
	return exitCode, execOutputBuf, nil
}

func (network *ServiceNetworkImpl) GetServiceIP(serviceId service_network_types.ServiceID) (net.IP, error) {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot get IP address; the service network has been destroyed")
	}

	registrationInfo, found := network.serviceRegistrationInfo[serviceId]
	if !found {
		return nil, stacktrace.NewError("Service with ID: '%v' does not exist", serviceId)
	}

	return registrationInfo.ipAddr, nil
}

func (network *ServiceNetworkImpl) GetServiceSuiteExecutionVolMntDirpath(serviceId service_network_types.ServiceID) (string, error) {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return "", stacktrace.NewError("Cannot get suite execution volume mount directory path; the service network has been destroyed")
	}

	runInfo, found := network.serviceRunInfo[serviceId]
	if !found {
		return "", stacktrace.NewError("No run information found for service with ID '%v'", serviceId)
	}

	return runInfo.suiteExecutionVolumeMountDirpath, nil
}


// ====================================================================================================
// 									   Private helper methods
// ====================================================================================================
func (network *ServiceNetworkImpl) removeServiceWithoutMutex(
		ctx context.Context,
		serviceId service_network_types.ServiceID,
		containerStopTimeout time.Duration) error {
	registrationInfo, foundRegistrationInfo := network.serviceRegistrationInfo[serviceId]
	if !foundRegistrationInfo {
		return stacktrace.NewError("No registration info found for service '%v'", serviceId)
	}
	network.topology.RemoveService(serviceId)
	delete(network.serviceRegistrationInfo, serviceId)

	// TODO PERF: Parallelize the shutdown of the service container and the sidecar container

	runInfo, foundRunInfo := network.serviceRunInfo[serviceId]
	if foundRunInfo {
		serviceContainerId := runInfo.containerId
		// Make a best-effort attempt to stop the service container
		logrus.Debugf("Stopping container ID '%v' for service ID '%v'...", serviceContainerId, serviceId)
		if err := network.dockerManager.StopContainer(ctx, serviceContainerId, containerStopTimeout); err != nil {
			return stacktrace.Propagate(err, "An error occurred stopping the container with ID %v", serviceContainerId)
		}
		delete(network.serviceRunInfo, serviceId)
		logrus.Debugf("Successfully stopped container ID")
	}
	network.freeIpAddrTracker.ReleaseIpAddr(registrationInfo.ipAddr)

	sidecar, foundSidecar := network.networkingSidecars[serviceId]
	if network.isPartitioningEnabled && foundSidecar {
		// NOTE: As of 2020-12-31, we don't need to update the iptables of the other services in the network to
		//  clear the now-removed service's IP because:
		// 	 a) nothing is using it so it doesn't do anything and
		//	 b) all service's iptables get overwritten on the next Add/Repartition call
		// If we ever do incremental iptables though, we'll need to fix all the other service's iptables here!
		if err := network.networkingSidecarManager.Destroy(ctx, sidecar); err != nil {
			return stacktrace.Propagate(err, "An error occurred destroying the sidecar for service with ID '%v'", serviceId)
		}
		delete(network.networkingSidecars, serviceId)
		logrus.Debugf("Successfully removed sidecar attached to service with ID '%v'", serviceId)
	}

	return nil
}

/*
Updates the iptables of the services with the given IDs to match the target blocklists

NOTE: This is not thread-safe, so it must be within a function that locks mutex!
 */
func updateIpTables(
		ctx context.Context,
		targetBlocklists map[service_network_types.ServiceID]*service_network_types.ServiceIDSet,
		serviceRegistrationInfo map[service_network_types.ServiceID]serviceRegistrationInfo,
		networkingSidecars map[service_network_types.ServiceID]networking_sidecar.NetworkingSidecar) error {
	// TODO PERF: Run the container updates in parallel, with the container being modified being the most important
	for serviceId, newBlocklist := range targetBlocklists {
		allIpsToBlock := []net.IP{}
		for _, serviceIdToBlock := range newBlocklist.Elems() {
			infoForService, found := serviceRegistrationInfo[serviceIdToBlock]
			if !found {
				return stacktrace.NewError(
					"Service with ID '%v' needs to block service with ID '%v', but the latter " +
						"doesn't have service registration info (i.e. an IP) associated with it",
					serviceId,
					serviceIdToBlock)
			}
			allIpsToBlock = append(allIpsToBlock, infoForService.ipAddr)
		}

		sidecar, found := networkingSidecars[serviceId]
		if !found {
			return stacktrace.NewError(
				"Need to update iptables of service with ID '%v', but the service doesn't have a sidecar",
				serviceId)
		}
		if err := sidecar.UpdateIpTables(ctx, allIpsToBlock); err != nil {
			return stacktrace.Propagate(
				err,
				"An error occurred updating the iptables for service '%v'",
				serviceId)
		}
	}
	return nil
}


