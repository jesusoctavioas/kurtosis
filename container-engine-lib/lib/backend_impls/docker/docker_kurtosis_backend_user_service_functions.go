package docker

import (
	"bytes"
	"context"
	"fmt"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/docker_log_streaming_readcloser"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/docker_manager"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/docker_manager/types"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/docker_operation_parallelizer"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/object_attributes_provider"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/docker_port_spec_serializer"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/label_key_consts"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/label_value_consts"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/container_status"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/enclave"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/exec_result"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/files_artifacts_expansion"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/port_spec"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/service"
	"github.com/kurtosis-tech/container-engine-lib/lib/concurrent_writer"
	"github.com/kurtosis-tech/free-ip-addr-tracker-lib/lib"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"io"
	"net"
	"strings"
	"time"
)

/*

      *************************************************************************************************
      * See the documentation on the KurtosisBackend interface for the Kurtosis service state diagram *
      *************************************************************************************************

                                      DOCKER SERVICES IMPLEMENTATION
States:
- REGISTERED: a ServiceGUID has been allocated in an in-memory map of DockerKurtosisBackend, allocating it to a
	ServiceGUID and an IP. No container for the service yet exists. We need to store the IP address in memory because
	there's no Docker object representing a registered, but not yet started, service.
- RUNNING: a user container is running using the previously-allocated IP address
- STOPPED: a user container was running, but is now stopped
- DESTROYED: the in-memory ServiceGUID registration no longer exists, and the container (if one was ever started) has
	been removed. The IP address that was allocated has been returned to the pool.

State transitions:
- RegisterService: an IP address is allocated, a ServiceGUID is generated, and both are stored in the in-memory map of
	DockerKurtosisBackend.
- StartService: the previously-allocated IP address of the registration is consumed to start a user container.
- StopServices: the containers of the matching services are killed (rather than deleted) so that logs are still accessible.
- DestroyServices: any container that was started is destroyed, the registration is removed from the in-memory map, and
	the IP address is freed.

Implementation notes:
- Because we're keeping an in-memory map, a mutex is important to keep it thread-safe. IT IS VERY IMPORTANT THAT ALL METHODS
	WHICH USE THE IN-MEMORY SERVICE REGISTRATION MAP LOCK THE MUTEX!
- Because an in-memory map is being kept, it means that any operation that requires that map will ONLY be doable via the API
	container (because if the CLI were to do the same operation, it wouldn't have the in-memory map and things would be weird).
- We might think "let's just push everything through the API container", but certain operations should still work even
	when the API container is stopped (e.g. 'enclave inspect' in the CLI). This means that KurtosisBackend code powering
	'enclave inspect' needs to a) not flow through the API container and b) not use the in-memory map
- Thus, we had to make it such that things like GetServices *don't* use the in-memory map. This led to some restrictions (e.g.
	we can't actually return a Service object with a status indicating if it's registered or not because doing so requires
	the in-memory map which means it must be done through the API container).
- The implementation we settled on is that, ServiceRegistrations are like service "stubs" returned by RegisterService,
	but they're identified by a ServiceGUID just like a full service. StartService "upgrades" a ServiceRegistration into a full
	Service object.

The benefits of this implementation:
- We can get the IP address before the service is started, which is crucial because certain user containers actually need
	to know their own IP when they start (e.g. Ethereum and Avalanche nodes require a flag to be passed in with their own IP)
- We can stop a service and free its memory/CPU resources while still preserving the logs for users
- We can call the GetServices method (that the CLI needs) without the API container running
*/

const (
	shouldGetStoppedContainersWhenGettingServiceInfo = true

	shouldFollowContainerLogsWhenExpanderHasError = false

	expanderContainerSuccessExitCode = 0
)

// We'll try to use the nicer-to-use shells first before we drop down to the lower shells
var commandToRunWhenCreatingUserServiceShell = []string{
	"sh",
	"-c",
	"if command -v 'bash' > /dev/null; then echo \"Found bash on container; creating bash shell...\"; bash; else echo \"No bash found on container; dropping down to sh shell...\"; sh; fi",
}

// NOTE: Normally we'd have a "canonical" resource here, where that resource is always guaranteed to exist. For Kurtosis services,
// we want this to be the container engine's representation of a user service registration. Unfortunately, Docker has no way
// of representing a user service registration, so we store them in an in-memory map on the DockerKurtosisBackend. Therefore, an
// entry in that map is actually the canonical representation, which means that any of these fields could be nil!
type userServiceDockerResources struct {
	serviceContainer *types.Container

	// Will never be nil but may be empty if no expander volumes exist
	expanderVolumeNames []string
}

func (backend *DockerKurtosisBackend) RegisterUserService(ctx context.Context, enclaveId enclave.EnclaveID, serviceId service.ServiceID) (*service.ServiceRegistration, error) {
	backend.serviceRegistrationMutex.Lock()
	defer backend.serviceRegistrationMutex.Unlock()

	freeIpAddrProvider, found := backend.enclaveFreeIpProviders[enclaveId]
	if !found {
		return nil, stacktrace.NewError(
			"Received a request to register service with ID '%v' in enclave '%v', but no free IP address provider was "+
				"defined for this enclave; this likely means that the registration request is being called where it shouldn't "+
				"be (i.e. outside the API container)",
			serviceId,
			enclaveId,
		)
	}

	registrationsForEnclave, found := backend.serviceRegistrations[enclaveId]
	if !found {
		return nil, stacktrace.NewError(
			"No service registrations are being tracked for enclave '%v'; this likely means that the registration request is being called where it shouldn't "+
				"be (i.e. outside the API container)",
			enclaveId,
		)
	}

	ipAddr, err := freeIpAddrProvider.GetFreeIpAddr()
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting a free IP address to give to service '%v' in enclave '%v'", serviceId, enclaveId)
	}
	shouldFreeIp := true
	defer func() {
		if shouldFreeIp {
			freeIpAddrProvider.ReleaseIpAddr(ipAddr)
		}
	}()

	// TODO Switch this, and all other GUIDs, to a UUID??
	guid := service.ServiceGUID(fmt.Sprintf(
		"%v-%v",
		serviceId,
		time.Now().Unix(),
	))
	registration := service.NewServiceRegistration(
		serviceId,
		guid,
		enclaveId,
		ipAddr,
	)

	registrationsForEnclave[guid] = registration
	shouldRemoveRegistration := true
	defer func() {
		if shouldRemoveRegistration {
			delete(registrationsForEnclave, guid)
		}
	}()

	shouldFreeIp = false
	shouldRemoveRegistration = false
	return registration, nil
}

// Registers a user service for each given serviceId, allocating each an IP and ServiceGUID
func (backend *DockerKurtosisBackend) RegisterUserServices(ctx context.Context, enclaveId enclave.EnclaveID, serviceIds map[service.ServiceID]bool, ) (map[service.ServiceID]*service.ServiceRegistration, map[service.ServiceID]error, error){
	return nil, nil, stacktrace.NewError("REGISTER USER SERVICES METHOD IS UNIMPLEMENTED. DON'T USE IT")
}


func (backend *DockerKurtosisBackend) StartUserService(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	serviceGuid service.ServiceGUID,
	containerImageName string,
	privatePorts map[string]*port_spec.PortSpec,
	publicPorts map[string]*port_spec.PortSpec, //TODO this is a huge hack to temporarily enable static ports for NEAR until we have a more productized solution
	entrypointArgs []string,
	cmdArgs []string,
	envVars map[string]string,
	filesArtifactsExpansion *files_artifacts_expansion.FilesArtifactsExpansion,
	cpuAllocationMillicpus uint64,
	memoryAllocationMegabytes uint64,
) (*service.Service, error) {

	//Sanity check for port bindings
	//TODO this is a huge hack to temporarily enable static ports for NEAR until we have a more productized solution
	if publicPorts != nil && len(publicPorts) > 0 {

		if len(privatePorts) != len(publicPorts) {
			return nil, stacktrace.NewError("The received private ports length and the public ports length are not equal, received '%v' private ports and '%v' public ports", len(privatePorts), len(publicPorts))
		}

		for portId, privatePortSpec := range privatePorts {
			if _, found := publicPorts[portId]; !found {
				return nil, stacktrace.NewError("Expected to receive public port with ID '%v' bound to private port number '%v', but it was not found", portId, privatePortSpec.GetNumber())
			}
		}
	}

	backend.serviceRegistrationMutex.Lock()
	defer backend.serviceRegistrationMutex.Unlock()

	enclaveNetwork, err := backend.getEnclaveNetworkByEnclaveId(ctx, enclaveId)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting enclave network by enclave ID '%v'", enclaveId)
	}
	enclaveNetworkId := enclaveNetwork.GetId()

	// Needed for files artifacts expansion container
	freeIpAddrProvider, found := backend.enclaveFreeIpProviders[enclaveId]
	if !found {
		return nil, stacktrace.NewError(
			"Received a request to start service with GUID '%v' in enclave '%v', but no free IP address provider was "+
				"defined for this enclave; this likely means that the start request is being called where it shouldn't "+
				"be (i.e. outside the API container)",
			serviceGuid,
			enclaveId,
		)
	}

	// Find the registration
	registrationsForEnclave, found := backend.serviceRegistrations[enclaveId]
	if !found {
		return nil, stacktrace.NewError(
			"No service registrations are being tracked for enclave '%v'; this likely means that the start service request is being called where it shouldn't "+
				"be (i.e. outside the API container)",
			enclaveId,
		)
	}
	serviceRegistration, found := registrationsForEnclave[serviceGuid]
	if !found {
		return nil, stacktrace.NewError(
			"Cannot start service '%v' because no preexisting registration has been made for the service",
			serviceGuid,
		)
	}
	serviceId := serviceRegistration.GetID()
	privateIpAddr := serviceRegistration.GetPrivateIP()

	// Find if a container has been associated with the registration yet
	preexistingServicesFilters := &service.ServiceFilters{
		GUIDs: map[service.ServiceGUID]bool{
			serviceGuid: true,
		},
	}
	preexistingServices, _, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, preexistingServicesFilters)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting preexisting containers for service '%v'", serviceGuid)
	}
	if len(preexistingServices) > 0 {
		return nil, stacktrace.Propagate(err, "Cannot start service '%v'; a container already exists for the service", serviceGuid)
	}

	enclaveObjAttrsProvider, err := backend.objAttrsProvider.ForEnclave(enclaveId)
	if err != nil {
		return nil, stacktrace.Propagate(err, "Couldn't get an object attribute provider for enclave '%v'", enclaveId)
	}

	volumeMounts := map[string]string{}
	shouldDeleteVolumes := true
	if filesArtifactsExpansion != nil {
		candidateVolumeMounts, err := backend.doFilesArtifactExpansionAndGetUserServiceVolumes(
			ctx,
			serviceGuid,
			enclaveObjAttrsProvider,
			freeIpAddrProvider,
			enclaveNetworkId,
			filesArtifactsExpansion.ExpanderImage,
			filesArtifactsExpansion.ExpanderEnvVars,
			filesArtifactsExpansion.ExpanderDirpathsToServiceDirpaths,
		)
		if err != nil {
			return nil, stacktrace.Propagate(
				err,
				"An error occurred doing files artifacts expansion to get user service volumes",
			)
		}
		defer func() {
			if shouldDeleteVolumes {
				for volumeName := range candidateVolumeMounts {
					// Use background context so we delete these even if input context was cancelled
					if err := backend.dockerManager.RemoveVolume(context.Background(), volumeName); err != nil {
						logrus.Errorf("Starting the service failed so we tried to delete files artifact expansion volume '%v' that we created, but doing so threw an error:\n%v", volumeName, err)
						logrus.Errorf("You'll need to delete volume '%v' manually!", volumeName)
					}
				}
			}
		}()
		volumeMounts = candidateVolumeMounts
	}

	containerAttrs, err := enclaveObjAttrsProvider.ForUserServiceContainer(
		serviceId,
		serviceGuid,
		privateIpAddr,
		privatePorts,
	)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while trying to get the user service container attributes for user service with GUID '%v'", serviceGuid)
	}
	containerName := containerAttrs.GetName()

	labelStrs := map[string]string{}
	for labelKey, labelValue := range containerAttrs.GetLabels() {
		labelStrs[labelKey.GetString()] = labelValue.GetString()
	}

	dockerUsedPorts := map[nat.Port]docker_manager.PortPublishSpec{}
	for portId, privatePortSpec := range privatePorts {
		dockerPort, err := transformPortSpecToDockerPort(privatePortSpec)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred converting private port spec '%v' to a Docker port", portId)
		}
		//TODO this is a huge hack to temporarily enable static ports for NEAR until we have a more productized solution
		if publicPorts != nil && len(publicPorts) > 0 {
			publicPortSpec, found := publicPorts[portId]
			if !found {
				return nil, stacktrace.NewError("Expected to receive public port with ID '%v' bound to private port number '%v', but it was not found", portId, privatePortSpec.GetNumber())
			}
			dockerUsedPorts[dockerPort] = docker_manager.NewManualPublishingSpec(publicPortSpec.GetNumber())
		} else {
			dockerUsedPorts[dockerPort] = docker_manager.NewAutomaticPublishingSpec()
		}
	}

	createAndStartArgsBuilder := docker_manager.NewCreateAndStartContainerArgsBuilder(
		containerImageName,
		containerName.GetString(),
		enclaveNetworkId,
	).WithStaticIP(
		privateIpAddr,
	).WithUsedPorts(
		dockerUsedPorts,
	).WithEnvironmentVariables(
		envVars,
	).WithLabels(
		labelStrs,
	).WithAlias(
		string(serviceId),
	).WithCPUAllocationMillicpus(
		cpuAllocationMillicpus,
	).WithMemoryAllocationMegabytes(
		memoryAllocationMegabytes,
	)

	if entrypointArgs != nil {
		createAndStartArgsBuilder.WithEntrypointArgs(entrypointArgs)
	}
	if cmdArgs != nil {
		createAndStartArgsBuilder.WithCmdArgs(cmdArgs)
	}
	if volumeMounts != nil {
		createAndStartArgsBuilder.WithVolumeMounts(volumeMounts)
	}

	createAndStartArgs := createAndStartArgsBuilder.Build()

	// Best-effort pull attempt
	if err = backend.dockerManager.PullImage(ctx, containerImageName); err != nil {
		logrus.Warnf("Failed to pull the latest version of user service container image '%v'; you may be running an out-of-date version", containerImageName)
	}

	containerId, hostMachinePortBindings, err := backend.dockerManager.CreateAndStartContainer(ctx, createAndStartArgs)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred starting the user service container for user service with GUID '%v'", serviceGuid)
	}
	shouldKillContainer := true
	defer func() {
		if shouldKillContainer {
			// TODO switch to removing the container, so that the service registration is still viable?
			// NOTE: We use the background context here so that the kill will still go off even if the reason for
			// the failure was the original context being cancelled
			if err := backend.dockerManager.KillContainer(context.Background(), containerId); err != nil {
				logrus.Errorf(
					"Launching user service container '%v' with container ID '%v' didn't complete successfully so we "+
						"tried to kill the container we started, but doing so exited with an error:\n%v",
					containerName.GetString(),
					containerId,
					err)
				logrus.Errorf("ACTION REQUIRED: You'll need to manually stop user service container with ID '%v'!!!!!!", containerId)
			}
		}
	}()

	_, _, maybePublicIp, maybePublicPortSpecs, err := getIpAndPortInfoFromContainer(
		containerName.GetString(),
		labelStrs,
		hostMachinePortBindings,
	)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the public IP and ports from container '%v'", containerName)
	}

	result := service.NewService(
		serviceRegistration,
		container_status.ContainerStatus_Running,
		privatePorts,
		maybePublicIp,
		maybePublicPortSpecs,
	)

	shouldDeleteVolumes = false
	shouldKillContainer = false
	return result, nil
}

func (backend *DockerKurtosisBackend) StartUserServices(ctx context.Context, enclaveId enclave.EnclaveID, services map[service.ServiceGUID]*service.ServiceConfig) (map[service.ServiceGUID]service.Service, map[service.ServiceGUID]error, error){
	return nil, nil, stacktrace.NewError("START USER SERVICES METHOD IS UNIMPLEMENTED. DON'T USE IT")
}

func (backend *DockerKurtosisBackend) GetUserServices(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	filters *service.ServiceFilters,
) (
	map[service.ServiceGUID]*service.Service,
	error,
) {
	userServices, _, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, filters)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting user services matching filters '%+v'", filters)
	}
	return userServices, nil
}

func (backend *DockerKurtosisBackend) GetUserServiceLogs(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	filters *service.ServiceFilters,
	shouldFollowLogs bool,
) (
	map[service.ServiceGUID]io.ReadCloser,
	map[service.ServiceGUID]error,
	error,
) {
	_, allDockerResources, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting user services matching filters '%+v'", filters)
	}

	//TODO use concurrency to improve perf
	successfulUserServicesLogs := map[service.ServiceGUID]io.ReadCloser{}
	erroredUserServices := map[service.ServiceGUID]error{}
	shouldCloseLogStreams := true
	for guid, resourcesForService := range allDockerResources {
		container := resourcesForService.serviceContainer
		if container == nil {
			erroredUserServices[guid] = stacktrace.NewError("Cannot get logs for service '%v' as it has no container", guid)
			continue
		}

		rawDockerLogStream, err := backend.dockerManager.GetContainerLogs(ctx, container.GetId(), shouldFollowLogs)
		if err != nil {
			serviceError := stacktrace.Propagate(err, "An error occurred getting logs for container '%v' for user service with GUID '%v'", container.GetName(), guid)
			erroredUserServices[guid] = serviceError
			continue
		}
		defer func() {
			if shouldCloseLogStreams {
				rawDockerLogStream.Close()
			}
		}()

		demultiplexedLogStream := docker_log_streaming_readcloser.NewDockerLogStreamingReadCloser(rawDockerLogStream)
		defer func() {
			if shouldCloseLogStreams {
				demultiplexedLogStream.Close()
			}
		}()

		successfulUserServicesLogs[guid] = demultiplexedLogStream
	}

	shouldCloseLogStreams = false
	return successfulUserServicesLogs, erroredUserServices, nil
}

func (backend *DockerKurtosisBackend) PauseService(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	serviceGuid service.ServiceGUID,
) error {
	_, dockerResources, err := backend.getSingleUserServiceObjAndResourcesNoMutex(ctx, enclaveId, serviceGuid)
	if err != nil {
		return stacktrace.Propagate(err, "Failed to get information about service '%v' from Kurtosis backend.", serviceGuid)
	}
	container := dockerResources.serviceContainer
	if container == nil {
		return stacktrace.NewError("Cannot pause service '%v' as it doesn't have a container to pause", serviceGuid)
	}
	if err = backend.dockerManager.PauseContainer(ctx, container.GetId()); err != nil {
		return stacktrace.Propagate(err, "Failed to pause container '%v' for service '%v' ", container.GetName(), serviceGuid)
	}
	return nil
}

func (backend *DockerKurtosisBackend) UnpauseService(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	serviceGuid service.ServiceGUID,
) error {
	_, dockerResources, err := backend.getSingleUserServiceObjAndResourcesNoMutex(ctx, enclaveId, serviceGuid)
	if err != nil {
		return stacktrace.Propagate(err, "Failed to get information about service '%v' from Kurtosis backend.", serviceGuid)
	}
	container := dockerResources.serviceContainer
	if container == nil {
		return stacktrace.NewError("Cannot unpause service '%v' as it doesn't have a container to pause", serviceGuid)
	}
	if err = backend.dockerManager.UnpauseContainer(ctx, container.GetId()); err != nil {
		return stacktrace.Propagate(err, "Failed to unppause container '%v' for service '%v' ", container.GetName(), serviceGuid)
	}
	return nil
}

// TODO Switch these to streaming so that huge command outputs don't blow up the API container memory
// NOTE: This function will block while the exec is ongoing; if we need more perf we can make it async
func (backend *DockerKurtosisBackend) RunUserServiceExecCommands(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	userServiceCommands map[service.ServiceGUID][]string,
) (
	map[service.ServiceGUID]*exec_result.ExecResult,
	map[service.ServiceGUID]error,
	error,
) {
	userServiceGuids := map[service.ServiceGUID]bool{}
	for userServiceGuid := range userServiceCommands {
		userServiceGuids[userServiceGuid] = true
	}

	filters := &service.ServiceFilters{
		GUIDs: userServiceGuids,
	}
	_, allDockerResources, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting user services matching filters '%+v'", filters)
	}

	// TODO Parallelize to increase perf
	succesfulUserServiceExecResults := map[service.ServiceGUID]*exec_result.ExecResult{}
	erroredUserServiceGuids := map[service.ServiceGUID]error{}
	for guid, commandArgs := range userServiceCommands {
		dockerResources, found := allDockerResources[guid]
		if !found {
			erroredUserServiceGuids[guid] = stacktrace.NewError(
				"Cannot execute command '%+v' on service '%v' because no Docker resources were found for it",
				commandArgs,
				guid,
			)
			continue
		}
		container := dockerResources.serviceContainer

		execOutputBuf := &bytes.Buffer{}
		exitCode, err := backend.dockerManager.RunExecCommand(
			ctx,
			container.GetId(),
			commandArgs,
			execOutputBuf,
		)
		if err != nil {
			wrappedErr := stacktrace.Propagate(
				err,
				"An error occurred executing command '%+v' on container '%v' for user service '%v'",
				commandArgs,
				container.GetName(),
				guid,
			)
			erroredUserServiceGuids[guid] = wrappedErr
			continue
		}
		newExecResult := exec_result.NewExecResult(exitCode, execOutputBuf.String())
		succesfulUserServiceExecResults[guid] = newExecResult
	}

	return succesfulUserServiceExecResults, erroredUserServiceGuids, nil
}

func (backend *DockerKurtosisBackend) GetConnectionWithUserService(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	serviceGuid service.ServiceGUID,
) (
	net.Conn,
	error,
) {
	_, serviceDockerResources, err := backend.getSingleUserServiceObjAndResourcesNoMutex(ctx, enclaveId, serviceGuid)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting service object and Docker resources for service '%v' in enclave '%v'", serviceGuid, enclaveId)
	}
	container := serviceDockerResources.serviceContainer

	hijackedResponse, err := backend.dockerManager.CreateContainerExec(ctx, container.GetId(), commandToRunWhenCreatingUserServiceShell)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting a shell on user service with GUID '%v' in enclave '%v'", serviceGuid, enclaveId)
	}

	newConnection := hijackedResponse.Conn

	return newConnection, nil
}

// It returns io.ReadCloser which is a tar stream. It's up to the caller to close the reader.
func (backend *DockerKurtosisBackend) CopyFilesFromUserService(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	serviceGuid service.ServiceGUID,
	srcPathOnContainer string,
	output io.Writer,
) error {
	_, serviceDockerResources, err := backend.getSingleUserServiceObjAndResourcesNoMutex(ctx, enclaveId, serviceGuid)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting user service with GUID '%v' in enclave with ID '%v'", serviceGuid, enclaveId)
	}
	container := serviceDockerResources.serviceContainer

	tarStreamReadCloser, err := backend.dockerManager.CopyFromContainer(ctx, container.GetId(), srcPathOnContainer)
	if err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred copying content from sourcepath '%v' in container '%v' for user service '%v' in enclave '%v'",
			srcPathOnContainer,
			container.GetName(),
			serviceGuid,
			enclaveId,
		)
	}
	defer tarStreamReadCloser.Close()

	if _, err := io.Copy(output, tarStreamReadCloser); err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred copying the bytes of TAR'd up files at '%v' on service '%v' to the output",
			srcPathOnContainer,
			serviceGuid,
		)
	}

	return nil
}

func (backend *DockerKurtosisBackend) StopUserServices(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	filters *service.ServiceFilters,
) (
	resultSuccessfulServiceGUIDs map[service.ServiceGUID]bool,
	resultErroredServiceGUIDs map[service.ServiceGUID]error,
	resultErr error,
) {
	allServiceObjs, allDockerResources, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting user services matching filters '%+v'", filters)
	}

	servicesToStopByContainerId := map[string]interface{}{}
	for guid, serviceResources := range allDockerResources {
		serviceObj, found := allServiceObjs[guid]
		if !found {
			// Should never happen; there should be a 1:1 mapping between service_objects:docker_resources by GUID
			return nil, nil, stacktrace.NewError("No service object found for service '%v' that had Docker resources", guid)
		}
		servicesToStopByContainerId[serviceResources.serviceContainer.GetId()] = serviceObj
	}

	// TODO PLEAAASE GO GENERICS... but we can't use 1.18 yet because it'll break all Kurtosis clients :(
	var dockerOperation docker_operation_parallelizer.DockerOperation = func(
		ctx context.Context,
		dockerManager *docker_manager.DockerManager,
		dockerObjectId string,
	) error {
		if err := dockerManager.KillContainer(ctx, dockerObjectId); err != nil {
			return stacktrace.Propagate(err, "An error occurred killing user service container with ID '%v'", dockerObjectId)
		}
		return nil
	}

	successfulGuidStrs, erroredGuidStrs, err := docker_operation_parallelizer.RunDockerOperationInParallelForKurtosisObjects(
		ctx,
		servicesToStopByContainerId,
		backend.dockerManager,
		extractServiceGUIDFromServiceObj,
		dockerOperation,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred killing user service containers matching filters '%+v'", filters)
	}

	successfulGuids := map[service.ServiceGUID]bool{}
	for guidStr := range successfulGuidStrs {
		successfulGuids[service.ServiceGUID(guidStr)] = true
	}

	erroredGuids := map[service.ServiceGUID]error{}
	for guidStr, err := range erroredGuidStrs {
		erroredGuids[service.ServiceGUID(guidStr)] = stacktrace.Propagate(
			err,
			"An error occurred stopping service '%v'",
			guidStr,
		)
	}

	return successfulGuids, erroredGuids, nil
}

/*
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!! WARNING !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
This code is INCREDIBLY tricky, as a result of:
1) Needing to do service registrations to get an IP address before the service container is started
2) Docker not having a canonical way to represent a service registration-before-container-started,
   which requires us to use an in-memory registration map

        Be VERY careful when modifying this code, and ideally get Kevin's eyes on it!!
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!! WARNING !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
*/
func (backend *DockerKurtosisBackend) DestroyUserServices(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	filters *service.ServiceFilters,
) (
	resultSuccessfulGuids map[service.ServiceGUID]bool,
	resultErroredGuids map[service.ServiceGUID]error,
	resultErr error,
) {
	// Write lock, because we'll be modifying the service registration info
	backend.serviceRegistrationMutex.Lock()
	defer backend.serviceRegistrationMutex.Unlock()

	freeIpAddrTrackerForEnclave, found := backend.enclaveFreeIpProviders[enclaveId]
	if !found {
		return nil, nil, stacktrace.NewError(
			"Cannot destroy services in enclave '%v' because no free IP address tracker is registered for it; this likely "+
				"means that the destroy user services call is being made from somewhere it shouldn't be (i.e. outside the API contianer)",
			enclaveId,
		)
	}

	registrationsForEnclave, found := backend.serviceRegistrations[enclaveId]
	if !found {
		return nil, nil, stacktrace.NewError(
			"No service registrations are being tracked for enclave '%v', so we cannot get service registrations matching filters: %+v",
			enclaveId,
			filters,
		)
	}

	// We filter registrations here because a registration is the canonical resource for a Kurtosis user service - no registration,
	// no Kurtosis service - and not all registrations will have Docker resources
	matchingRegistrations := map[service.ServiceGUID]*service.ServiceRegistration{}
	for guid, registration := range registrationsForEnclave {
		if filters.GUIDs != nil && len(filters.GUIDs) > 0 {
			if _, found := filters.GUIDs[registration.GetGUID()]; !found {
				continue
			}
		}

		if filters.IDs != nil && len(filters.IDs) > 0 {
			if _, found := filters.IDs[registration.GetID()]; !found {
				continue
			}
		}

		matchingRegistrations[guid] = registration
	}

	// NOTE: This may end up with less results here than we have registrations, if the user registered but did not start a service,
	// though we should never end up with _more_ Docker resources
	allServiceObjs, allDockerResources, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting user services matching filters '%+v'", filters)
	}

	if len(allServiceObjs) > len(matchingRegistrations) || len(allDockerResources) > len(matchingRegistrations) {
		return nil, nil, stacktrace.NewError("Found more Docker resources matching the following filters than service registrations; this is a bug in Kurtosis: %+v", filters)
	}

	// TODO Refactor getMatchingUserServiceObjsAndDockerResourcesNoMutex to return a single package object so we don't need to do this
	// Before deleting anything, verify we have the same keys in both maps
	if len(allServiceObjs) != len(allDockerResources) {
		return nil, nil, stacktrace.NewError(
			"Expected the number of services to remove (%v), and the number of services for which we have Docker resources for (%v), to be the same but were different!",
			len(allServiceObjs),
			len(allDockerResources),
		)
	}
	for serviceGuid := range allServiceObjs {
		if _, found := allDockerResources[serviceGuid]; !found {
			return nil, nil, stacktrace.NewError(
				"Have service object to remove '%v', which doesn't have corresponding Docker resources; this is a "+
					"bug in Kurtosis",
				serviceGuid,
			)
		}
	}

	registrationsToDeregister := map[service.ServiceGUID]*service.ServiceRegistration{}

	// Find the registrations which don't have any Docker resources and immediately add them to the list of stuff to deregister
	for guid, registration := range matchingRegistrations {
		if _, doesRegistrationHaveResources := allDockerResources[guid]; doesRegistrationHaveResources {
			// We'll deregister registrations-with-resources if and only if we can successfully remove their resources
			continue
		}

		// If the status filter is specified, don't deregister any registrations-without-resources
		if filters.Statuses != nil && len(filters.Statuses) > 0 {
			continue
		}

		registrationsToDeregister[guid] = registration
	}

	// Now try removing all the registrations-with-resources
	successfulResourceRemovalGuids, erroredResourceRemovalGuids, err := backend.removeUserServiceDockerResources(
		ctx,
		allServiceObjs,
		allDockerResources,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(
			err,
			"An error occurred trying to remove user service Docker resources",
		)
	}

	erroredGuids := map[service.ServiceGUID]error{}
	for guid, err := range erroredResourceRemovalGuids {
		erroredGuids[guid] = stacktrace.Propagate(
			err,
			"An error occurred destroying Docker resources for service '%v'",
			guid,
		)
	}

	for guid := range successfulResourceRemovalGuids {
		registrationsToDeregister[guid] = matchingRegistrations[guid]
	}

	// Finalize deregistration
	successfulGuids := map[service.ServiceGUID]bool{}
	for guid, registration := range registrationsToDeregister {
		freeIpAddrTrackerForEnclave.ReleaseIpAddr(registration.GetPrivateIP())
		delete(registrationsForEnclave, guid)
	}

	return successfulGuids, erroredGuids, nil
}

// ====================================================================================================
//                                     Private Helper Methods
// ====================================================================================================
// Gets the service objects & Docker resources for services matching the given filters
// NOTE: Does not use registration information so does not need the mutex!
func (backend *DockerKurtosisBackend) getMatchingUserServiceObjsAndDockerResourcesNoMutex(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	filters *service.ServiceFilters,
) (
	map[service.ServiceGUID]*service.Service,
	map[service.ServiceGUID]*userServiceDockerResources,
	error,
) {
	matchingDockerResources, err := backend.getMatchingUserServiceDockerResources(ctx, enclaveId, filters.GUIDs)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting matching user service resources")
	}

	matchingServiceObjs, err := getUserServiceObjsFromDockerResources(enclaveId, matchingDockerResources)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting Kurtosis service objects from user service Docker resources")
	}

	resultServiceObjs := map[service.ServiceGUID]*service.Service{}
	resultDockerResources := map[service.ServiceGUID]*userServiceDockerResources{}
	for guid, serviceObj := range matchingServiceObjs {
		if filters.GUIDs != nil && len(filters.GUIDs) > 0 {
			if _, found := filters.GUIDs[serviceObj.GetRegistration().GetGUID()]; !found {
				continue
			}
		}

		if filters.IDs != nil && len(filters.IDs) > 0 {
			if _, found := filters.IDs[serviceObj.GetRegistration().GetID()]; !found {
				continue
			}
		}

		if filters.Statuses != nil && len(filters.Statuses) > 0 {
			if _, found := filters.Statuses[serviceObj.GetStatus()]; !found {
				continue
			}
		}

		dockerResources, found := matchingDockerResources[guid]
		if !found {
			// This should never happen; the Services map and the Docker resources maps should have the same GUIDs
			return nil, nil, stacktrace.Propagate(
				err,
				"Needed to return Docker resources for service with GUID '%v', but none was "+
					"found; this is a bug in Kurtosis",
				guid,
			)
		}

		resultServiceObjs[guid] = serviceObj
		resultDockerResources[guid] = dockerResources
	}
	return resultServiceObjs, resultDockerResources, nil
}

func (backend *DockerKurtosisBackend) getMatchingUserServiceDockerResources(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	maybeGuidsToMatch map[service.ServiceGUID]bool,
) (map[service.ServiceGUID]*userServiceDockerResources, error) {
	result := map[service.ServiceGUID]*userServiceDockerResources{}

	// Grab services, INDEPENDENT OF volumes
	userServiceContainerSearchLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():         label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveIDDockerLabelKey.GetString():     string(enclaveId),
		label_key_consts.ContainerTypeDockerLabelKey.GetString(): label_value_consts.UserServiceContainerTypeDockerLabelValue.GetString(),
	}
	userServiceContainers, err := backend.dockerManager.GetContainersByLabels(ctx, userServiceContainerSearchLabels, shouldGetStoppedContainersWhenGettingServiceInfo)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting user service containers in enclave '%v' by labels: %+v", enclaveId, userServiceContainerSearchLabels)
	}

	for _, container := range userServiceContainers {
		serviceGuidStr, found := container.GetLabels()[label_key_consts.GUIDDockerLabelKey.GetString()]
		if !found {
			return nil, stacktrace.NewError("Found user service container '%v' that didn't have expected GUID label '%v'", container.GetId(), label_key_consts.GUIDDockerLabelKey.GetString())
		}
		serviceGuid := service.ServiceGUID(serviceGuidStr)

		if maybeGuidsToMatch != nil && len(maybeGuidsToMatch) > 0 {
			if _, found := maybeGuidsToMatch[serviceGuid]; !found {
				continue
			}
		}

		resourceObj, found := result[serviceGuid]
		if !found {
			resourceObj = &userServiceDockerResources{}
		}
		resourceObj.serviceContainer = container
		result[serviceGuid] = resourceObj
	}

	// Grab volumes, INDEPENDENT OF whether there any containers
	filesArtifactExpansionVolumeSearchLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():      label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveIDDockerLabelKey.GetString():  string(enclaveId),
		label_key_consts.VolumeTypeDockerLabelKey.GetString(): label_value_consts.FilesArtifactExpansionVolumeTypeDockerLabelValue.GetString(),
	}
	matchingFilesArtifactExpansionVolumes, err := backend.dockerManager.GetVolumesByLabels(ctx, filesArtifactExpansionVolumeSearchLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting files artifact expansion volumes in enclave '%v' by labels: %+v", enclaveId, filesArtifactExpansionVolumeSearchLabels)
	}

	for _, volume := range matchingFilesArtifactExpansionVolumes {
		serviceGuidStr, found := volume.Labels[label_key_consts.UserServiceGUIDDockerLabelKey.GetString()]
		if !found {
			return nil, stacktrace.NewError("Found files artifact expansion volume '%v' that didn't have expected service GUID label '%v'", volume.Name, label_key_consts.UserServiceGUIDDockerLabelKey.GetString())
		}
		serviceGuid := service.ServiceGUID(serviceGuidStr)

		if maybeGuidsToMatch != nil && len(maybeGuidsToMatch) > 0 {
			if _, found := maybeGuidsToMatch[serviceGuid]; !found {
				continue
			}
		}

		resourceObj, found := result[serviceGuid]
		if !found {
			resourceObj = &userServiceDockerResources{}
		}
		resourceObj.expanderVolumeNames = append(resourceObj.expanderVolumeNames, volume.Name)
		result[serviceGuid] = resourceObj
	}

	return result, nil
}

func getUserServiceObjsFromDockerResources(
	enclaveId enclave.EnclaveID,
	allDockerResources map[service.ServiceGUID]*userServiceDockerResources,
) (map[service.ServiceGUID]*service.Service, error) {
	result := map[service.ServiceGUID]*service.Service{}

	// If we have an entry in the map, it means there's at least one Docker resource
	for serviceGuid, resources := range allDockerResources {
		container := resources.serviceContainer

		// If we don't have a container, we don't have the service ID label which means we can't actually construct a Service object
		// The only case where this would happen is if, during deletion, we delete the container but an error occurred deleting the volumes
		if container == nil {
			return nil, stacktrace.NewError(
				"Service '%v' has Docker resources but not a container; this indicates that there the service's "+
					"container was deleted but errors occurred deleting the rest of the resources",
				serviceGuid,
			)
		}
		containerName := container.GetName()
		containerLabels := container.GetLabels()

		serviceIdStr, found := containerLabels[label_key_consts.IDDockerLabelKey.GetString()]
		if !found {
			return nil, stacktrace.NewError("Expected to find label '%v' on container '%v' but label was missing", label_key_consts.IDDockerLabelKey.GetString(), containerName)
		}
		serviceId := service.ServiceID(serviceIdStr)

		privateIp, privatePorts, maybePublicIp, maybePublicPorts, err := getIpAndPortInfoFromContainer(
			containerName,
			containerLabels,
			container.GetHostPortBindings(),
		)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting IP & port info from container '%v'", container.GetName())
		}

		registration := service.NewServiceRegistration(
			serviceId,
			serviceGuid,
			enclaveId,
			privateIp,
		)

		containerStatus := container.GetStatus()
		isContainerRunning, found := isContainerRunningDeterminer[containerStatus]
		if !found {
			return nil, stacktrace.NewError("No is-running determination found for status '%v' for container '%v'", containerStatus.String(), containerName)
		}
		serviceStatus := container_status.ContainerStatus_Stopped
		if isContainerRunning {
			serviceStatus = container_status.ContainerStatus_Running
		}

		result[serviceGuid] = service.NewService(
			registration,
			serviceStatus,
			privatePorts,
			maybePublicIp,
			maybePublicPorts,
		)
	}
	return result, nil
}

// TODO Extract this to DockerKurtosisBackend and use it everywhere, for Engines, Modules, and API containers?
func getIpAndPortInfoFromContainer(
	containerName string,
	labels map[string]string,
	hostMachinePortBindings map[nat.Port]*nat.PortBinding,
) (
	resultPrivateIp net.IP,
	resultPrivatePortSpecs map[string]*port_spec.PortSpec,
	resultPublicIp net.IP,
	resultPublicPortSpecs map[string]*port_spec.PortSpec,
	resultErr error,
) {
	privateIpAddrStr, found := labels[label_key_consts.PrivateIPDockerLabelKey.GetString()]
	if !found {
		return nil, nil, nil, nil, stacktrace.NewError("Expected to find label '%v' on container '%v' but label was missing", label_key_consts.PrivateIPDockerLabelKey.GetString(), containerName)
	}
	privateIp := net.ParseIP(privateIpAddrStr)
	if privateIp == nil {
		return nil, nil, nil, nil, stacktrace.NewError("Couldn't parse private IP string '%v' on container '%v' to an IP address", privateIpAddrStr, containerName)
	}

	serializedPortSpecs, found := labels[label_key_consts.PortSpecsDockerLabelKey.GetString()]
	if !found {
		return nil, nil, nil, nil, stacktrace.NewError(
			"Expected to find port specs label '%v' on container '%v' but none was found",
			containerName,
			label_key_consts.PortSpecsDockerLabelKey.GetString(),
		)
	}

	privatePortSpecs, err := docker_port_spec_serializer.DeserializePortSpecs(serializedPortSpecs)
	if err != nil {
		if err != nil {
			return nil, nil, nil, nil, stacktrace.Propagate(err, "Couldn't deserialize port spec string '%v'", serializedPortSpecs)
		}
	}

	var containerPublicIp net.IP
	var publicPortSpecs map[string]*port_spec.PortSpec
	if hostMachinePortBindings == nil || len(hostMachinePortBindings) == 0 {
		return privateIp, privatePortSpecs, containerPublicIp, publicPortSpecs, nil
	}

	for portId, privatePortSpec := range privatePortSpecs {
		portPublicIp, publicPortSpec, err := getPublicPortBindingFromPrivatePortSpec(privatePortSpec, hostMachinePortBindings)
		if err != nil {
			return nil, nil, nil, nil, stacktrace.Propagate(
				err,
				"An error occurred getting public port spec for private port '%v' with spec '%v/%v' on container '%v'",
				portId,
				privatePortSpec.GetNumber(),
				privatePortSpec.GetProtocol().String(),
				containerName,
			)
		}

		if containerPublicIp == nil {
			containerPublicIp = portPublicIp
		} else {
			if !containerPublicIp.Equal(portPublicIp) {
				return nil, nil, nil, nil, stacktrace.NewError(
					"Private port '%v' on container '%v' yielded a public IP '%v', which doesn't agree with "+
						"previously-seen public IP '%v'",
					portId,
					containerName,
					portPublicIp.String(),
					containerPublicIp.String(),
				)
			}
		}

		if publicPortSpecs == nil {
			publicPortSpecs = map[string]*port_spec.PortSpec{}
		}
		publicPortSpecs[portId] = publicPortSpec
	}

	return privateIp, privatePortSpecs, containerPublicIp, publicPortSpecs, nil
}

// NOTE: Does not use registration information so does not need the mutex!
func (backend *DockerKurtosisBackend) getSingleUserServiceObjAndResourcesNoMutex(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	userServiceGuid service.ServiceGUID,
) (
	*service.Service,
	*userServiceDockerResources,
	error,
) {
	filters := &service.ServiceFilters{
		GUIDs: map[service.ServiceGUID]bool{
			userServiceGuid: true,
		},
	}
	userServices, dockerResources, err := backend.getMatchingUserServiceObjsAndDockerResourcesNoMutex(ctx, enclaveId, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting user services using filters '%v'", filters)
	}
	numOfUserServices := len(userServices)
	if numOfUserServices == 0 {
		return nil, nil, stacktrace.NewError("No user service with GUID '%v' in enclave with ID '%v' was found", userServiceGuid, enclaveId)
	}
	if numOfUserServices > 1 {
		return nil, nil, stacktrace.NewError("Expected to find only one user service with GUID '%v' in enclave with ID '%v', but '%v' was found", userServiceGuid, enclaveId, numOfUserServices)
	}

	var resultService *service.Service
	for _, resultService = range userServices {
	}

	var resultDockerResources *userServiceDockerResources
	for _, resultDockerResources = range dockerResources {
	}

	return resultService, resultDockerResources, nil
}

func extractServiceGUIDFromServiceObj(uncastedObj interface{}) (string, error) {
	castedObj, ok := uncastedObj.(*service.Service)
	if !ok {
		return "", stacktrace.NewError("An error occurred downcasting the user service object")
	}
	return string(castedObj.GetRegistration().GetGUID()), nil
}

func (backend *DockerKurtosisBackend) doFilesArtifactExpansionAndGetUserServiceVolumes(
	ctx context.Context,
	serviceGuid service.ServiceGUID,
	objAttrsProvider object_attributes_provider.DockerEnclaveObjectAttributesProvider,
	freeIpAddrProvider *lib.FreeIpAddrTracker,
	enclaveNetworkId string,
	expanderImage string,
	expanderEnvVars map[string]string,
	expanderMountpointsToServiceMountpoints map[string]string,
) (map[string]string, error) {
	requestedExpanderMountpoints := map[string]bool{}
	for expanderMountpoint := range expanderMountpointsToServiceMountpoints {
		requestedExpanderMountpoints[expanderMountpoint] = true
	}
	expanderMountpointsToVolumeNames, err := backend.createFilesArtifactsExpansionVolumes(
		ctx,
		serviceGuid,
		objAttrsProvider,
		requestedExpanderMountpoints,
	)
	if err != nil {
		return nil, stacktrace.Propagate(
			err,
			"Couldn't create files artifact expansion volumes for requested expander mounpoints: %+v",
			requestedExpanderMountpoints,
		)
	}
	shouldDeleteVolumes := true
	defer func() {
		if shouldDeleteVolumes {
			for _, volumeName := range expanderMountpointsToVolumeNames {
				// Use background context so we delete these even if input context was cancelled
				if err := backend.dockerManager.RemoveVolume(context.Background(), volumeName); err != nil {
					logrus.Errorf("Running the expansion failed so we tried to delete files artifact expansion volume '%v' that we created, but doing so threw an error:\n%v", volumeName, err)
					logrus.Errorf("You'll need to delete volume '%v' manually!", volumeName)
				}
			}
		}
	}()

	if err := backend.runFilesArtifactsExpander(
		ctx,
		serviceGuid,
		objAttrsProvider,
		freeIpAddrProvider,
		expanderImage,
		expanderEnvVars,
		enclaveNetworkId,
		expanderMountpointsToVolumeNames,
	); err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred running files artifacts expander for service '%v'",
			serviceGuid,
		)
	}

	userServiceVolumeMounts := map[string]string{}
	for expanderMountpoint, userServiceMountpoint := range expanderMountpointsToServiceMountpoints {
		volumeName, found := expanderMountpointsToVolumeNames[expanderMountpoint]
		if !found {
			return nil, stacktrace.NewError(
				"Found expander mountpoint '%v' for which no expansion volume was created; this should never happen "+
					"and is a bug in Kurtosis",
				expanderMountpoint,
			)
		}
		userServiceVolumeMounts[volumeName] = userServiceMountpoint
	}

	shouldDeleteVolumes = false
	return userServiceVolumeMounts, nil
}

// Runs a single expander container which expands one or more files artifacts into multiple volumes
// NOTE: It is the caller's responsibility to handle the volumes that get returned
func (backend *DockerKurtosisBackend) runFilesArtifactsExpander(
	ctx context.Context,
	serviceGuid service.ServiceGUID,
	objAttrProvider object_attributes_provider.DockerEnclaveObjectAttributesProvider,
	freeIpAddrProvider *lib.FreeIpAddrTracker,
	image string,
	envVars map[string]string,
	enclaveNetworkId string,
	mountpointsToVolumeNames map[string]string,
) error {
	containerAttrs, err := objAttrProvider.ForFilesArtifactsExpanderContainer(serviceGuid)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while trying to get the files artifact expander container attributes for service '%v'", serviceGuid)
	}
	containerName := containerAttrs.GetName().GetString()
	containerLabels := map[string]string{}
	for labelKey, labelValue := range containerAttrs.GetLabels() {
		containerLabels[labelKey.GetString()] = labelValue.GetString()
	}

	volumeMounts := map[string]string{}
	for mountpointOnExpander, volumeName := range mountpointsToVolumeNames {
		volumeMounts[volumeName] = mountpointOnExpander
	}

	ipAddr, err := freeIpAddrProvider.GetFreeIpAddr()
	if err != nil {
		return stacktrace.Propagate(err, "Couldn't get a free IP to give the expander container '%v'", containerName)
	}
	defer freeIpAddrProvider.ReleaseIpAddr(ipAddr)

	createAndStartArgs := docker_manager.NewCreateAndStartContainerArgsBuilder(
		image,
		containerName,
		enclaveNetworkId,
	).WithStaticIP(
		ipAddr,
	).WithEnvironmentVariables(
		envVars,
	).WithVolumeMounts(
		volumeMounts,
	).WithLabels(
		containerLabels,
	).Build()
	containerId, _, err := backend.dockerManager.CreateAndStartContainer(ctx, createAndStartArgs)
	if err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred creating files artifacts expander container '%v' for service '%v'",
			containerName,
			serviceGuid,
		)
	}
	defer func() {
		// We destroy the expander container, rather than leaving it around, so that we clean up the resource we created
		// in this function (meaning the caller doesn't have to do it)
		// We can do this because if an error occurs, we'll capture the logs of the container in the error we return
		// to the user
		if destroyContainerErr := backend.dockerManager.RemoveContainer(ctx, containerId); destroyContainerErr != nil {
			logrus.Errorf(
				"We tried to remove the expander container '%v' with ID '%v' that we started, but doing so threw an error:\n%v",
				containerName,
				containerId,
				destroyContainerErr,
			)
			logrus.Errorf("ACTION REQUIRED: You'll need to remove files artifacts expander container '%v' manually", containerName)
		}
	}()

	exitCode, err := backend.dockerManager.WaitForExit(ctx, containerId)
	if err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred waiting for files artifacts expander container '%v' to exit",
			containerName,
		)
	}
	if exitCode != expanderContainerSuccessExitCode {
		containerLogsBlockStr, err := backend.getFilesArtifactsExpanderContainerLogsBlockStr(
			ctx,
			containerId,
		)
		if err != nil {
			return stacktrace.NewError(
				"Files artifacts expander container '%v' for service '%v' finished with non-%v exit code '%v' so we tried "+
					"to get the logs, but doing so failed with an error:\n%v",
				containerName,
				serviceGuid,
				expanderContainerSuccessExitCode,
				exitCode,
				err,
			)
		}
		return stacktrace.NewError(
			"Files artifacts expander container '%v' for service '%v' finished with non-%v exit code '%v' and logs:\n%v",
			containerName,
			serviceGuid,
			expanderContainerSuccessExitCode,
			exitCode,
			containerLogsBlockStr,
		)
	}

	return nil
}

// This seems like a lot of effort to go through to get the logs of a failed container, but easily seeing the reason an expander
// container has failed has proven to be very useful
func (backend *DockerKurtosisBackend) getFilesArtifactsExpanderContainerLogsBlockStr(
	ctx context.Context,
	containerId string,
) (string, error) {
	containerLogsReadCloser, err := backend.dockerManager.GetContainerLogs(ctx, containerId, shouldFollowContainerLogsWhenExpanderHasError)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred getting the logs for expander container with ID '%v'", containerId)
	}
	defer containerLogsReadCloser.Close()

	buffer := &bytes.Buffer{}
	concurrentBuffer := concurrent_writer.NewConcurrentWriter(buffer)

	// TODO Push this down into GetContainerLogs!!!! This code actually has a bug where it won't work if the container is a TTY
	//  container; the proper checking logic can be seen in the 'enclave dump' functions but should all be abstracted by GetContainerLogs
	//  The only reason I'm not doing it right now is because we have the huge ETH deadline tomorrow and I don't have time for any
	//  nice-to-have refactors (ktoday, 2022-05-22)
	if _, err := stdcopy.StdCopy(concurrentBuffer, concurrentBuffer, containerLogsReadCloser); err != nil {
		return "", stacktrace.Propagate(
			err,
			"An error occurred copying logs to memory for files artifact expander container '%v'",
			containerId,
		)
	}

	wrappedContainerLogsStrBuilder := strings.Builder{}
	wrappedContainerLogsStrBuilder.WriteString(fmt.Sprintf(
		">>>>>>>>>>>>>>>>>>>>>>>>>>>>>> Logs for container '%v' <<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<\n",
		containerId,
	))
	wrappedContainerLogsStrBuilder.WriteString(buffer.String())
	wrappedContainerLogsStrBuilder.WriteString("\n")
	wrappedContainerLogsStrBuilder.WriteString(fmt.Sprintf(
		">>>>>>>>>>>>>>>>>>>>>>>>>>>> End logs for container '%v' <<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<<",
		containerId,
	))

	return wrappedContainerLogsStrBuilder.String(), nil
}

// Takes in a list of mountpoints on the expander container that the expander container wants populated with volumes,
// creates one volume per mountpoint location, and returns the volume_name -> mountpoint map that the container
// can use when starting
func (backend *DockerKurtosisBackend) createFilesArtifactsExpansionVolumes(
	ctx context.Context,
	serviceGuid service.ServiceGUID,
	enclaveObjAttrsProvider object_attributes_provider.DockerEnclaveObjectAttributesProvider,
	allMountpointsExpanderWants map[string]bool,
) (map[string]string, error) {
	shouldDeleteVolumes := true
	result := map[string]string{}
	for mountpointExpanderWants := range allMountpointsExpanderWants {
		volumeAttrs, err := enclaveObjAttrsProvider.ForSingleFilesArtifactExpansionVolume(serviceGuid)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred creating files artifact expansion volume for service '%v'", serviceGuid)
		}
		volumeNameStr := volumeAttrs.GetName().GetString()
		volumeLabelsStrs := map[string]string{}
		for key, value := range volumeAttrs.GetLabels() {
			volumeLabelsStrs[key.GetString()] = value.GetString()
		}
		if err := backend.dockerManager.CreateVolume(
			ctx,
			volumeAttrs.GetName().GetString(),
			volumeLabelsStrs,
		); err != nil {
			return nil, stacktrace.Propagate(
				err,
				"An error occurred creating files artifact expansion volume for service '%v' that's intended to be mounted "+
					"on the expander container at path '%v'",
				serviceGuid,
				mountpointExpanderWants,
			)
		}
		//goland:noinspection GoDeferInLoop
		defer func() {
			if shouldDeleteVolumes {
				// Background context so we still run this even if the input context was cancelled
				if err := backend.dockerManager.RemoveVolume(context.Background(), volumeNameStr); err != nil {
					logrus.Warnf(
						"Creating files artifact expansion volumes didn't complete successfully so we tried to delete volume '%v' that we created, but doing so threw an error:\n%v",
						volumeNameStr,
						err,
					)
					logrus.Warnf("You'll need to clean up volume '%v' manually!", volumeNameStr)
				}
			}
		}()

		result[mountpointExpanderWants] = volumeNameStr
	}
	shouldDeleteVolumes = false
	return result, nil
}

/*
Destroys the Docker containers and makes a best-effort attempt to destroy the volumes

NOTE: We make a best-effort attempt to delete files artifact expansion volumes for a service, but there's still the
possibility that some will get leaked! There's unfortunately no way around this though because:

1) We delete the canonical resource (the user container) before the volumes out of necessity
   since Docker won't let us delete volumes unless no containers are using them
2) We can't undo container deletion
3) Normally whichever resource is created first (volumes) is the one we'd use as the canonical resource,
   but we can't do that since we're not guaranteed to have volumes (because a service may not have requested any
   files artifacts).

Therefore, we just make a best-effort attempt to clean up the volumes and leak the rest, though it's not THAT
big of a deal since they'll be deleted when the enclave gets deleted.
*/
func (backend *DockerKurtosisBackend) removeUserServiceDockerResources(
	ctx context.Context,
	serviceObjectsToRemove map[service.ServiceGUID]*service.Service,
	resourcesToRemove map[service.ServiceGUID]*userServiceDockerResources,
) (map[service.ServiceGUID]bool, map[service.ServiceGUID]error, error) {

	erroredGuids := map[service.ServiceGUID]error{}

	// Before deleting anything, verify we have the same keys in both maps
	if len(serviceObjectsToRemove) != len(resourcesToRemove) {
		return nil, nil, stacktrace.NewError(
			"Expected the number of services to remove (%v), and the number of services for which we have Docker resources for (%v), to be the same but were different!",
			len(serviceObjectsToRemove),
			len(resourcesToRemove),
		)
	}
	for serviceGuid := range serviceObjectsToRemove {
		if _, found := resourcesToRemove[serviceGuid]; !found {
			return nil, nil, stacktrace.NewError(
				"Have service object to remove '%v', which doesn't have corresponding Docker resources; this is a "+
					"bug in Kurtosis",
				serviceGuid,
			)
		}
	}

	uncastedKurtosisObjectsToRemoveByContainerId := map[string]interface{}{}
	for serviceGuid, resources := range resourcesToRemove {
		// Safe to skip the is-found check because we verified the map keys are identical earlier
		serviceObj := serviceObjectsToRemove[serviceGuid]

		containerId := resources.serviceContainer.GetId()
		uncastedKurtosisObjectsToRemoveByContainerId[containerId] = serviceObj
	}

	// TODO Simplify this with Go generics
	var dockerOperation docker_operation_parallelizer.DockerOperation = func(
		ctx context.Context,
		dockerManager *docker_manager.DockerManager,
		dockerObjectId string,
	) error {
		if err := dockerManager.RemoveContainer(ctx, dockerObjectId); err != nil {
			return stacktrace.Propagate(err, "An error occurred removing user service container with ID '%v'", dockerObjectId)
		}
		return nil
	}

	successfulContainerRemoveGuidStrs, erroredContainerRemoveGuidStrs, err := docker_operation_parallelizer.RunDockerOperationInParallelForKurtosisObjects(
		ctx,
		uncastedKurtosisObjectsToRemoveByContainerId,
		backend.dockerManager,
		extractServiceGUIDFromServiceObj,
		dockerOperation,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred removing user service containers in parallel")
	}

	for guidStr, err := range erroredContainerRemoveGuidStrs {
		erroredGuids[service.ServiceGUID(guidStr)] = stacktrace.Propagate(
			err,
			"An error occurred destroying container for service '%v'",
			guidStr,
		)
	}

	// TODO Parallelize if we need more perf (but we shouldn't, since removing volumes way faster than containers)
	successfulVolumeRemovalGuids := map[service.ServiceGUID]bool{}
	for serviceGuidStr := range successfulContainerRemoveGuidStrs {
		serviceGuid := service.ServiceGUID(serviceGuidStr)

		// Safe to skip the is-found check because we verified that the maps have the same keys earlier
		resources := resourcesToRemove[serviceGuid]

		failedVolumeErrStrs := []string{}
		for _, volumeName := range resources.expanderVolumeNames {
			/*
				We try to delete as many volumes as we can here, rather than ejecting on failure, because any volumes not
				deleted here will be leaked! There's unfortunately no way around this though because:

				 1) we've already deleted the canonical resource (the user container) out of necessity
				    since Docker won't let us delete volumes unless no containers are using them
				 2) we can't undo container deletion
				 3) normally whichever resource is created first (volumes) is the one we'd use as the canonical resource,
				    but we can't do that since we're not guaranteed to have volumes

				Therefore, we just make a best-effort attempt to clean up the volumes and leak the rest :(
			*/
			if err := backend.dockerManager.RemoveVolume(ctx, volumeName); err != nil {
				errStrBuilder := strings.Builder{}
				errStrBuilder.WriteString(fmt.Sprintf(
					">>>>>>>>>>>>>>>>>> Removal error for volume %v <<<<<<<<<<<<<<<<<<<<<<<<<<<\n",
					volumeName,
				))
				errStrBuilder.WriteString(err.Error())
				errStrBuilder.WriteString("\n")
				errStrBuilder.WriteString(fmt.Sprintf(
					">>>>>>>>>>>>>>> End removal error for volume %v <<<<<<<<<<<<<<<<<<<<<<<<<<",
					volumeName,
				))
				failedVolumeErrStrs = append(failedVolumeErrStrs, errStrBuilder.String())
			}
		}

		if len(failedVolumeErrStrs) > 0 {
			erroredGuids[serviceGuid] = stacktrace.NewError(
				"Errors occurred removing volumes for service '%v'\n"+
					"ACTION REQUIRED: You will need to manually remove these volumes, else they will stay around until the enclave is destroyed!\n"+
					"%v",
				serviceGuid,
				strings.Join(failedVolumeErrStrs, "\n\n"),
			)
			continue
		}
		successfulVolumeRemovalGuids[serviceGuid] = true
	}

	successGuids := successfulVolumeRemovalGuids
	return successGuids, erroredGuids, nil
}
