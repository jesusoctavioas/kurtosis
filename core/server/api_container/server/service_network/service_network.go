/*
 * Copyright (c) 2021 - present Kurtosis Technologies Inc.
 * All Rights Reserved.
 */

package service_network

import (
	"compress/gzip"
	"context"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/enclave"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/port_spec"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/service"
	"github.com/kurtosis-tech/kurtosis-core/server/api_container/server/service_network/networking_sidecar"
	"github.com/kurtosis-tech/kurtosis-core/server/api_container/server/service_network/partition_topology"
	"github.com/kurtosis-tech/kurtosis-core/server/api_container/server/service_network/service_network_types"
	"github.com/kurtosis-tech/kurtosis-core/server/api_container/server/service_network/user_service_launcher"
	"github.com/kurtosis-tech/kurtosis-core/server/commons/current_time_str_provider"
	"github.com/kurtosis-tech/kurtosis-core/server/commons/enclave_data_directory"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultPartitionId                       service_network_types.PartitionID = "default"
	startingDefaultConnectionPacketLossValue                                   = 0
)

/*
This is the in-memory representation of the service network that the API container will manipulate. To make
	any changes to the test network, this struct must be used.
*/
type ServiceNetwork struct {
	enclaveId enclave.EnclaveID

	// When the network is destroyed, all requests will fail
	// This ensures that when the initializer tells the API container to destroy everything, the still-running
	//  testsuite can't create more work
	isDestroyed bool // VERY IMPORTANT TO CHECK AT THE START OF EVERY METHOD!

	mutex *sync.Mutex // VERY IMPORTANT TO CHECK AT THE START OF EVERY METHOD!

	// Whether partitioning has been enabled for this particular test
	isPartitioningEnabled bool

	kurtosisBackend backend_interface.KurtosisBackend

	enclaveDataDir *enclave_data_directory.EnclaveDataDirectory

	userServiceLauncher *user_service_launcher.UserServiceLauncher

	topology *partition_topology.PartitionTopology

	networkingSidecars map[service.ServiceID]networking_sidecar.NetworkingSidecarWrapper

	networkingSidecarManager networking_sidecar.NetworkingSidecarManager


	// ----------------------------- Indexes to make common operations quick --------------------------------
	registrationsByServiceId map[service.ServiceID]*user_service_registration.UserServiceRegistration
	servicesByServiceId map[service.ServiceID]*service.Service
}

func NewServiceNetworkImpl(
	enclaveId enclave.EnclaveID,
	isPartitioningEnabled bool,
	kurtosisBackend backend_interface.KurtosisBackend,
	enclaveDataDir *enclave_data_directory.EnclaveDataDirectory,
	userServiceLauncher *user_service_launcher.UserServiceLauncher,
	networkingSidecarManager networking_sidecar.NetworkingSidecarManager,
) *ServiceNetwork {
	defaultPartitionConnection := partition_topology.PartitionConnection{
		PacketLossPercentage: startingDefaultConnectionPacketLossValue,
	}
	return &ServiceNetwork{
		enclaveId:             enclaveId,
		isDestroyed:           false,
		mutex:                 &sync.Mutex{},
		isPartitioningEnabled: isPartitioningEnabled,
		kurtosisBackend:       kurtosisBackend,
		enclaveDataDir:        enclaveDataDir,
		userServiceLauncher:   userServiceLauncher,
		topology: partition_topology.NewPartitionTopology(
			defaultPartitionId,
			defaultPartitionConnection,
		),
		networkingSidecars:       map[service.ServiceID]networking_sidecar.NetworkingSidecarWrapper{},
		networkingSidecarManager: networkingSidecarManager,
		registrationsByServiceId: map[service.ServiceID]*user_service_registration.UserServiceRegistration{},
		servicesByServiceId: map[service.ServiceID]*service.Service{},
	}
}

/*
Completely repartitions the network, throwing away the old topology
*/
func (network *ServiceNetwork) Repartition(
	ctx context.Context,
	newPartitionServices map[service_network_types.PartitionID]map[service.ServiceID]bool,
	newPartitionConnections map[service_network_types.PartitionConnectionID]partition_topology.PartitionConnection,
	newDefaultConnection partition_topology.PartitionConnection,
) error {
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

	servicePacketLossConfigurationsByServiceID, err := network.topology.GetServicePacketLossConfigurationsByServiceID()
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the packet loss configuration by service ID "+
			" after repartition, meaning that no partitions are actually being enforced!")
	}

	/*
	allInvolvedServiceIds := map[service.ServiceID]bool{}
	for serviceIdA, allServiceIdBs := range servicePacketLossConfigurationsByServiceID {
		allInvolvedServiceIds[serviceIdA] = true
		for serviceIdB := range allServiceIdBs {
			allInvolvedServiceIds[serviceIdB] = true
		}
	}

	allInvolvedRegistrations, err := network.kurtosisBackend.GetUserServiceRegistrations(
		ctx,
		&user_service_registration.UserServiceRegistrationFilters{
			ServiceIDs: allInvolvedServiceIds,
			EnclaveIDs: map[enclave.EnclaveID]bool{
				network.enclaveId: true,
			},
		},
	)

	registrationsByServiceId := map[service.ServiceID]*user_service_registration.UserServiceRegistration{}
	for _, registration := range allInvolvedRegistrations {
		registrationsByServiceId[registration.GetServiceID()] = registration
	}

	 */

	if err := updateTrafficControlConfiguration(ctx, servicePacketLossConfigurationsByServiceID, network.registrationsByServiceId, network.networkingSidecars); err != nil {
		return stacktrace.Propagate(err, "An error occurred updating the traffic control configuration to match the target service packet loss configurations after repartitioning")
	}
	return nil
}

// Registers a service for use with the network (creating the IPs and so forth), but doesn't start it
// If the partition ID is empty, registers the service with the default partition
func (network ServiceNetwork) RegisterService(
	ctx context.Context,
	serviceId service.ServiceID,
	partitionId service_network_types.PartitionID,
) (net.IP, error) {
	// TODO extract this into a wrapper function that can be wrapped around every service call (so we don't forget)
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot register service with ID '%v'; the service network has been destroyed", serviceId)
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

	serviceRegistration, err := network.kurtosisBackend.CreateUserServiceRegistration(
		ctx,
		network.enclaveId,
		serviceId,
	)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred creating service registration for service ID '%v'", serviceId)
	}
	shouldDestroyRegistration := true
	defer func() {
		if shouldDestroyRegistration {
			network.destroyServiceRegistrationBestEffort(serviceRegistration)
		}
	}()

	network.registrationsByServiceId[serviceId] = serviceRegistration
	shouldRemoveRegistrationFromIndex := true
	defer func() {
		if shouldRemoveRegistrationFromIndex {
			delete(network.registrationsByServiceId, serviceId)
		}
	}()

	if err := network.topology.AddService(serviceId, partitionId); err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred adding service with ID '%v' to partition '%v' in the topology",
			serviceId,
			partitionId,
		)
	}
	shouldRemoveTopologyAddition := true
	defer func() {
		if shouldRemoveTopologyAddition {
			network.topology.RemoveService(serviceId)
		}
	}()

	shouldDestroyRegistration = false
	shouldRemoveRegistrationFromIndex = false
	shouldRemoveTopologyAddition = false
	return serviceRegistration.GetIPAddress(), nil
}

// TODO add tests for this
/*
Starts a previously-registered but not-started service by creating it in a container

Returns:
	Mapping of port-used-by-service -> port-on-the-Docker-host-machine where the user can make requests to the port
		to access the port. If a used port doesn't have a host port bound, then the value will be nil.
*/
func (network *ServiceNetwork) StartService(
	ctx context.Context,
	serviceId service.ServiceID,
	imageName string,
	privatePorts map[string]*port_spec.PortSpec,
	entrypointArgs []string,
	cmdArgs []string,
	dockerEnvVars map[string]string,
	filesArtifactMountDirpaths map[service.FilesArtifactID]string,
) (
	resultMaybePublicIpAddr net.IP, // Will be nil if the service doesn't declare any private ports
	resultPublicPorts map[string]*port_spec.PortSpec,
	resultErr error,
) {
	// TODO extract this into a wrapper function that can be wrapped around every service call (so we don't forget)
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, nil, stacktrace.NewError("Cannot start container for service; the service network has been destroyed")
	}

	serviceRegistration, found := network.registrationsByServiceId[serviceId]
	if !found {
		return nil, nil, stacktrace.NewError("Cannot start service; no registration exists for service with ID '%v'", serviceId)
	}
	registrationGuid := serviceRegistration.GetGUID()

	// When partitioning is enabled, there's a race condition where:
	//   a) we need to start the service before we can launch the sidecar but
	//   b) we can't modify the qdisc configuration until the sidecar container is launched.
	// This means that there's a period of time at startup where the container might not be partitioned. We solve
	//  this by setting the packet loss config of the new service in the already-existing services' qdisc.
	// This means that when the new service is launched, even if its own qdisc isn't yet updated, all the services
	//  it would communicate are already dropping traffic to it before it even starts.
	if network.isPartitioningEnabled {
		servicePacketLossConfigurationsByServiceID, err := network.topology.GetServicePacketLossConfigurationsByServiceID()
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred getting the packet loss configuration by service ID "+
				" to know what packet loss updates to apply on the new node")
		}

		servicesPacketLossConfigurationsWithoutNewNode := map[service.ServiceID]map[service.ServiceID]float32{}
		for serviceIdInTopology, otherServicesPacketLossConfigs := range servicePacketLossConfigurationsByServiceID {
			if serviceId == serviceIdInTopology {
				continue
			}
			servicesPacketLossConfigurationsWithoutNewNode[serviceIdInTopology] = otherServicesPacketLossConfigs
		}

		if err := updateTrafficControlConfiguration(
			ctx,
			servicesPacketLossConfigurationsWithoutNewNode,
			network.registrationsByServiceId,
			network.networkingSidecars,
		); err != nil {
			return nil, nil, stacktrace.Propagate(
				err,
				"An error occurred updating the traffic control configuration of all the other services "+
					 "before adding the new service, meaning that the service wouldn't actually start in a partition",
			)
		}
		// TODO defer an undo somehow???
	}

	userService, err := network.userServiceLauncher.Launch(
		ctx,
		registrationGuid,
		network.enclaveId,
		imageName,
		privatePorts,
		entrypointArgs,
		cmdArgs,
		dockerEnvVars,
		filesArtifactMountDirpaths,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(
			err,
			"An error occurred creating service '%v'",
			serviceId,
		)
	}
	// TODO defer-undo the launch if a failure occurs?

	network.servicesByServiceId[serviceId] = userService
	shouldUndoServiceIdIndexAddition := true
	defer func() {
		if shouldUndoServiceIdIndexAddition {
			delete(network.servicesByServiceId, serviceId)
		}
	}()

	if network.isPartitioningEnabled {
		sidecar, err := network.networkingSidecarManager.Add(ctx, userService.GetGUID())
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred adding the networking sidecar")
		}
		network.networkingSidecars[serviceId] = sidecar

		if err := sidecar.InitializeTrafficControl(ctx); err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred initializing the newly-created networking-sidecar-traffic-control-qdisc-configuration")
		}

		// TODO Getting packet loss configuration by service ID is an expensive call and, as of 2021-11-23, we do it twice - the solution is to make
		//  Getting packet loss configuration by service ID not an expensive call
		servicePacketLossConfigurationsByServiceID, err := network.topology.GetServicePacketLossConfigurationsByServiceID()
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred getting the packet loss configuration by service ID "+
				" to know what packet loss updates to apply on the new node")
		}
		newNodeServicePacketLossConfiguration := servicePacketLossConfigurationsByServiceID[serviceId]
		updatesToApply := map[service.ServiceID]map[service.ServiceID]float32{
			serviceId: newNodeServicePacketLossConfiguration,
		}
		if err := updateTrafficControlConfiguration(ctx, updatesToApply, network.registrationsByServiceId, network.networkingSidecars); err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred applying the traffic control configuration on the new node to partition it "+
				"off from other nodes")
		}
	}

	shouldUndoServiceIdIndexAddition = false
	return userService.GetMaybePublicIP(), userService.GetMaybePublicPorts(), nil
}

func (network *ServiceNetwork) RemoveService(
	ctx context.Context,
	serviceId service.ServiceID,
	containerStopTimeout time.Duration,
) error {
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

// TODO we could switch this to be a bulk command; the backend would support it
func (network *ServiceNetwork) PauseService(
	ctx context.Context,
	serviceId service.ServiceID,
) error {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return stacktrace.NewError("Cannot run pause service; the service network has been destroyed")
	}

	serviceObj, found := network.servicesByServiceId[serviceId]
	if !found {
		return stacktrace.NewError("No service with ID '%v' exists in the network", serviceId)
	}

	if err := network.kurtosisBackend.PauseService(ctx, network.enclaveId, serviceObj.GetGUID()); err != nil {
		return stacktrace.Propagate(err,"Failed to pause service '%v'", serviceId)
	}
	return nil
}

func (network *ServiceNetwork) UnpauseService(
	ctx context.Context,
	serviceId service.ServiceID,
) error {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return stacktrace.NewError("Cannot run unpause service; the service network has been destroyed")
	}


	serviceObj, found := network.servicesByServiceId[serviceId]
	if !found {
		return stacktrace.NewError("No service with ID '%v' exists in the network", serviceId)
	}

	if err := network.kurtosisBackend.UnpauseService(ctx, network.enclaveId, serviceObj.GetGUID()); err != nil {
		return stacktrace.Propagate(err,"Failed to unpause service '%v'", serviceId)
	}
	return nil
}

func (network *ServiceNetwork) ExecCommand(
	ctx context.Context,
	serviceId service.ServiceID,
	command []string,
) (int32, string, error) {
	// NOTE: This will block all other operations while this command is running!!!! We might need to change this so it's
	// asynchronous
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return 0, "", stacktrace.NewError("Cannot run exec command; the service network has been destroyed")
	}

	serviceObj, found := network.servicesByServiceId[serviceId]
	if !found {
		return 0, "", stacktrace.NewError(
			"Could not run exec command '%v' against service '%v'; no container has been created for the service yet",
			command,
			serviceId)
	}

	// NOTE: This is a SYNCHRONOUS command, meaning that the entire network will be blocked until the command finishes
	// In the future, this will likely be insufficient

	serviceGuid := serviceObj.GetGUID()
	userServiceCommand := map[service.ServiceGUID][]string{
		serviceGuid: command,
	}

	successfulExecCommands, failedExecCommands, err := network.kurtosisBackend.RunUserServiceExecCommands(
		ctx,
		network.enclaveId,
		userServiceCommand)
	if err != nil {
		return 0, "", stacktrace.Propagate(
			err,
			"An error occurred calling kurtosis backend to exec command '%v' against service '%v'",
			command,
			serviceId)
	}
	if len(failedExecCommands) > 0 {
		serviceExecErrs := []string{}
		for serviceGUID, err := range failedExecCommands {
			wrappedErr := stacktrace.Propagate(
				err,
				"An error occurred attempting to run a command in a service with GUID `%v'",
				serviceGUID,
			)
			serviceExecErrs = append(serviceExecErrs, wrappedErr.Error())
		}
		return 0, "", stacktrace.NewError(
			"One or more errors occurred attempting to exec command(s) in the service(s): \n%v",
			strings.Join(
				serviceExecErrs,
				"\n\n",
			),
		)
	}

	execResult, isFound := successfulExecCommands[serviceGuid]
	if !isFound {
		return 0, "", stacktrace.NewError(
			"Unable to find result from running exec command '%v' against service '%v'",
			command,
			serviceGuid)
	}

	return execResult.GetExitCode(), execResult.GetOutput(), nil
}

func (network *ServiceNetwork) GetServiceRegistrationInfo(serviceId service.ServiceID) (
	privateIpAddr net.IP,
	resultErr error,
) {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, stacktrace.NewError("Cannot get registration info for service '%v'; the service network has been destroyed", serviceId)
	}

	registrationObj, found := network.registrationsByServiceId[serviceId]
	if !found {
		return nil, stacktrace.NewError("No registration information found for service with ID '%v'", serviceId)
	}

	return registrationObj.GetIPAddress(), nil
}


func (network *ServiceNetwork) GetServiceRunInfo(serviceId service.ServiceID) (
	privatePorts map[string]*port_spec.PortSpec,
	publicIpAddr net.IP,
	publicPorts map[string]*port_spec.PortSpec,
	resultErr error,
) {
	network.mutex.Lock()
	defer network.mutex.Unlock()
	if network.isDestroyed {
		return nil, nil, nil, stacktrace.NewError("Cannot get run info for service '%v'; the service network has been destroyed", serviceId)
	}

	serviceObj, found := network.servicesByServiceId[serviceId]
	if !found {
		return nil, nil, nil, stacktrace.NewError("No run information found for service with ID '%v'", serviceId)
	}
	return serviceObj.GetPrivatePorts(), serviceObj.GetMaybePublicIP(), serviceObj.GetMaybePublicPorts(), nil
}

func (network *ServiceNetwork) GetServiceIDs() map[service.ServiceID]bool {

	serviceIDs := make(map[service.ServiceID]bool, len(network.servicesByServiceId))

	for serviceId := range network.servicesByServiceId {
		if _, ok := serviceIDs[serviceId]; !ok {
			serviceIDs[serviceId] = true
		}
	}
	return serviceIDs
}

func (network *ServiceNetwork) CopyFromService(ctx context.Context, serviceId service.ServiceID, srcPath string) (string, error) {
	serviceObj, foundRunInfo := network.servicesByServiceId[serviceId]
	if !foundRunInfo {
		return "", stacktrace.NewError("No run information found for service with ID '%v'", serviceId)
	}
	serviceGuid := serviceObj.GetGUID()

	readCloser, err := network.kurtosisBackend.CopyFromUserService(ctx, network.enclaveId, serviceGuid, srcPath)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred copying source '%v' from user service with GUID '%v' in enclave with ID '%v'", srcPath, serviceGuid, network.enclaveId)
	}
	defer readCloser.Close()

	//Creates a new tgz file in a temporary directory
	tarGzipFileFilepath, err := gzipCompressFile(readCloser)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred creating new temporary tar-gzip-file")
	}

	//Then opens the .tgz file
	tarGzipFile, err := os.Open(tarGzipFileFilepath)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred opening file '%v'", tarGzipFileFilepath)
	}
	defer tarGzipFile.Close()

	store, err := network.enclaveDataDir.GetFilesArtifactStore()
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred getting the files artifact store")
	}

	//And finally pass it the .tgz file to the artifact file store
	uuid, err := store.StoreFile(tarGzipFile)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred while trying to store file '%v' into the artifact file store", tarGzipFileFilepath)
	}

	return uuid, nil
}

// ====================================================================================================
// 									   Private helper methods
// ====================================================================================================
func (network *ServiceNetwork) removeServiceWithoutMutex(
	ctx context.Context,
	serviceId service.ServiceID,
	containerStopTimeout time.Duration,
) error {
	registrationInfo, foundRegistrationInfo := network.registrationsByServiceId[serviceId]
	if !foundRegistrationInfo {
		return stacktrace.NewError("No registration info found for service '%v'", serviceId)
	}
	network.topology.RemoveService(serviceId)
	// TODO defer-undo

	delete(network.registrationsByServiceId, serviceId)
	shouldUndoRegistrationIndexRemoval := true
	defer func() {
		if shouldUndoRegistrationIndexRemoval {
			network.registrationsByServiceId[serviceId] = registrationInfo
		}
	}()

	// TODO PERF: Parallelize the shutdown of the service container and the sidecar container
	userService, foundUserServiceInfo := network.servicesByServiceId[serviceId]
	if foundUserServiceInfo {
		serviceGUID := userService.GetGUID()

		// Make a best-effort attempt to stop the service container
		logrus.Debugf("Stopping service with GUID '%v' for service ID '%v'...", serviceGUID, serviceId)
		_, failedToStopServiceErrs, err := network.kurtosisBackend.StopUserServices(
			ctx,
			getServiceByServiceGUIDFilter(serviceGUID),
		)
		if err != nil {
			return stacktrace.Propagate(err, "An error occurred calling the backend to stop service with GUID '%v'", serviceGUID)
		}
		if len(failedToStopServiceErrs) > 0 {
			serviceStopErrs := []string{}
			for failedToStopGuid, err := range failedToStopServiceErrs {
				wrappedErr := stacktrace.Propagate(
					err,
					"An error occurred stopping service with GUID `%v'",
					failedToStopGuid,
				)
				serviceStopErrs = append(serviceStopErrs, wrappedErr.Error())
			}
			return stacktrace.NewError(
				"One or more errors occurred stopping the service(s): \n%v",
				strings.Join(
					serviceStopErrs,
					"\n\n",
				),
			)
		}

		delete(network.servicesByServiceId, serviceId)
		// TODO defer-undo

		logrus.Debugf("Successfully stopped service GUID '%v'", serviceGUID)
	}

	sidecar, foundSidecar := network.networkingSidecars[serviceId]
	if network.isPartitioningEnabled && foundSidecar {
		// NOTE: As of 2020-12-31, we don't need to update the iptables of the other services in the network to
		//  clear the now-removed service's IP because:
		// 	 a) nothing is using it so it doesn't do anything and
		//	 b) all service's iptables get overwritten on the next Add/Repartition call
		// If we ever do incremental iptables though, we'll need to fix all the other service's iptables here!
		if err := network.networkingSidecarManager.Remove(ctx, sidecar); err != nil {
			return stacktrace.Propagate(err, "An error occurred destroying the sidecar for service with ID '%v'", serviceId)
		}
		delete(network.networkingSidecars, serviceId)
		logrus.Debugf("Successfully removed sidecar attached to service with ID '%v'", serviceId)
	}

	shouldUndoRegistrationIndexRemoval = true
	return nil
}

/*
Updates the traffic control configuration of the services with the given IDs to match the target services packet loss configuration

NOTE: This is not thread-safe, so it must be within a function that locks mutex!
*/
func updateTrafficControlConfiguration(
	ctx context.Context,
	targetServicePacketLossConfigs map[service.ServiceID]map[service.ServiceID]float32,
	registrationsByServiceId map[service.ServiceID]*user_service_registration.UserServiceRegistration,
	networkingSidecars map[service.ServiceID]networking_sidecar.NetworkingSidecarWrapper,
) error {

	// TODO PERF: Run the container updates in parallel, with the container being modified being the most important

	for serviceId, allOtherServicesPacketLossConfigurations := range targetServicePacketLossConfigs {
		allPacketLossPercentageForIpAddresses := map[string]float32{}
		for otherServiceId, otherServicePacketLossPercentage := range allOtherServicesPacketLossConfigurations {
			registration, found := registrationsByServiceId[otherServiceId]
			if !found {
				return stacktrace.NewError(
					"Service with ID '%v' needs to add packet loss configuration for service with ID '%v', but the latter "+
						"doesn't have service registration info (i.e. an IP) associated with it",
					serviceId,
					otherServiceId)
			}

			allPacketLossPercentageForIpAddresses[registration.GetIPAddress().String()] = otherServicePacketLossPercentage
		}

		sidecar, found := networkingSidecars[serviceId]
		if !found {
			return stacktrace.NewError(
				"Need to update qdisc configuration of service with ID '%v', but the service doesn't have a sidecar",
				serviceId)
		}

		if err := sidecar.UpdateTrafficControl(ctx, allPacketLossPercentageForIpAddresses); err != nil {
			return stacktrace.Propagate(
				err,
				"An error occurred updating the qdisc configuration for service '%v'",
				serviceId)
		}
	}
	return nil
}

func newServiceGUID(serviceID service.ServiceID) service.ServiceGUID {
	suffix := current_time_str_provider.GetCurrentTimeStr()
	return service.ServiceGUID(string(serviceID) + "-" + suffix)
}

func getServiceByServiceGUIDFilter(serviceGUID service.ServiceGUID) *service.ServiceFilters {
	return &service.ServiceFilters{
		GUIDs: map[service.ServiceGUID]bool{
			serviceGUID: true,
		},
	}
}

func gzipCompressFile(readCloser io.Reader) (resultFilepath string, resultErr error) {
	useDefaultDirectoryArg := ""
	withoutPatternArg := ""
	tgzFile, err := ioutil.TempFile(useDefaultDirectoryArg,withoutPatternArg)
	if err != nil {
		return "", stacktrace.Propagate(err,
			"There was an error creating a temporary file")
	}
	defer tgzFile.Close()

	gzipCompressingWriter := gzip.NewWriter(tgzFile)
	defer gzipCompressingWriter.Close()

	tarGzipFileFilepath := tgzFile.Name()
	if _, err := io.Copy(gzipCompressingWriter, readCloser); err != nil {
		return "", stacktrace.Propagate(err, "An error occurred copying content to file '%v'", tarGzipFileFilepath)
	}

	return tarGzipFileFilepath, nil
}

func (network *ServiceNetwork) destroyServiceRegistrationBestEffort(
	registration *user_service_registration.UserServiceRegistration,
) {
	serviceId := registration.GetServiceID()
	registrationGuid := registration.GetGUID()

	destroyRegistrationFilters := &user_service_registration.UserServiceRegistrationFilters{
		GUIDs: map[user_service_registration.UserServiceRegistrationGUID]bool{
			registrationGuid: true,
		},
	}
	// Use background context in case the input one is cancelled
	_, erroredRegistrations, err := network.kurtosisBackend.DestroyUserServiceRegistrations(context.Background(), destroyRegistrationFilters)
	var errToPrint error
	if err != nil {
		errToPrint = err
	} else if destroyErr, found := erroredRegistrations[registrationGuid]; found {
		errToPrint = destroyErr
	}
	if errToPrint != nil {
		logrus.Warnf(
			"Registering service with ID '%v' didn't complete successfully so we tried to destroy the " +
				"service registration object that we created, but doing so threw an error:\n%v",
			serviceId,
			errToPrint,
		)
		logrus.Warnf(
			"!!! ACTION REQUIRED !!! You'll need to manually destroy service registration object with GUID '%v'!!!",
			registrationGuid,
		)
	}
}