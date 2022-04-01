package docker

import (
	"context"
	"fmt"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/docker_manager"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/docker_manager/types"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/label_key_consts"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/label_value_consts"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/api_container"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/container_status"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/enclave"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/repl"
	"github.com/kurtosis-tech/container-engine-lib/lib/backend_interface/objects/shell"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"net"
	"strconv"
	"time"
)
const (
	// TODO Change this to base 16 to be more compact??
	guidBase = 10

	KurtosisSocketEnvVar          = "KURTOSIS_API_SOCKET"
	EnclaveIdEnvVar               = "ENCLAVE_ID"
	EnclaveDataMountDirpathEnvVar = "ENCLAVE_DATA_DIR_MOUNTPOINT"

	enclaveDataDirMountpointOnReplContainer = "/kurtosis-enclave-data"
)

func (backendCore *DockerKurtosisBackend) CreateRepl(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	containerImageName string,
	ipAddr net.IP, // TODO REMOVE THIS ONCE WE FIX THE STATIC IP PROBLEM!!
	stdoutFdInt int,
	bindMounts map[string]string,
)(
	*repl.Repl,
	error,
){

	replGuid := getReplGUID()

	enclaveNetwork, err := backendCore.getEnclaveNetworkByEnclaveId(ctx, enclaveId)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting enclave network by enclave ID '%v'", enclaveId)
	}

	enclaveObjAttrsProvider, err := backendCore.objAttrsProvider.ForEnclave(enclaveId)
	if err != nil {
		return nil, stacktrace.Propagate(err, "Couldn't get an object attribute provider for enclave with ID '%v'", enclaveId)
	}

	containerAttrs, err := enclaveObjAttrsProvider.ForInteractiveREPLContainer(replGuid)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while trying to get the repl container attributes for repl with GUID '%v'", replGuid)
	}
	containerName := containerAttrs.GetName()
	containerDockerLabels := containerAttrs.GetLabels()

	labels := map[string]string{}
	for dockerLabelKey, dockerLabelValue := range containerDockerLabels {
		labels[dockerLabelKey.GetString()] = dockerLabelValue.GetString()
	}

	windowSize, err := unix.IoctlGetWinsize(stdoutFdInt, unix.TIOCGWINSZ)
	if err != nil {
		return nil, stacktrace.NewError("An error occurred getting the current terminal window size")
	}
	interactiveModeTtySize := &docker_manager.InteractiveModeTtySize{
		Height: uint(windowSize.Row),
		Width:  uint(windowSize.Col),
	}

	apiContainerFilters := &api_container.APIContainerFilters{
		EnclaveIDs: map[enclave.EnclaveID]bool{
			enclaveId: true,
		},
	}

	apiContainers, err := backendCore.GetAPIContainers(ctx, apiContainerFilters)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting api container using filters '%+v'", apiContainerFilters)
	}
	if len(apiContainers) == 0 {
		return nil, stacktrace.NewError("No api container was found on enclave with ID '%v', it is not possible to create the repl container without this. ", enclaveId)
	}
	if len(apiContainers) > 1 {
		return nil, stacktrace.NewError("Expected to find only one api container on enclave with ID '%v', but '%v' was found; it should never happens it is a bug in Kurtosis", enclaveId, len(apiContainers))
	}

	apiContainer := apiContainers[enclaveId]

	kurtosisApiContainerSocket := fmt.Sprintf("%v:%v", apiContainer.GetPrivateIPAddress(), apiContainer.GetPrivateGRPCPort())

	createAndStartArgs := docker_manager.NewCreateAndStartContainerArgsBuilder(
		containerImageName,
		containerName.GetString(),
		enclaveNetwork.GetId(),
	).WithInteractiveModeTtySize(
		interactiveModeTtySize,
	).WithStaticIP(
		ipAddr,
	).WithEnvironmentVariables(map[string]string{
		KurtosisSocketEnvVar: kurtosisApiContainerSocket,
		EnclaveIdEnvVar: string(enclaveId),
		EnclaveDataMountDirpathEnvVar: enclaveDataDirMountpointOnReplContainer,
	}).WithBindMounts(
		bindMounts,
	).WithVolumeMounts(map[string]string{
		string(enclaveId): enclaveDataDirMountpointOnReplContainer,
	}).WithLabels(
		labels,
	).Build()

	// Best-effort pull attempt
	if err = backendCore.dockerManager.PullImage(ctx, containerName.GetString()); err != nil {
		logrus.Warnf("Failed to pull the latest version of the repl container image '%v'; you may be running an out-of-date version", containerName.GetString())
	}

	_, _, err = backendCore.dockerManager.CreateAndStartContainer(ctx, createAndStartArgs)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred starting the repl container")
	}

	newRepl, err := getReplObjectFromContainerInfo(labels, types.ContainerStatus_Running)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting repl object from container info with labels '%+v' and status '%v'", labels, types.ContainerStatus_Running)
	}

	return newRepl, nil
}

func (backendCore *DockerKurtosisBackend) Attach(
	ctx context.Context,
	enclaveId enclave.EnclaveID,
	replGuid repl.ReplGUID,
)(
	*shell.Shell,
	error,
){

	filters := &repl.ReplFilters{
		EnclaveIDs: map[enclave.EnclaveID]bool{
			enclaveId: true,
		},
		GUIDs: map[repl.ReplGUID]bool{
			replGuid: true,
		},
	}

	repls, err := backendCore.getMatchingRepls(ctx, filters)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting repls matching filters '%+v'", filters)
	}
	numOfRepls := len(repls)
	if numOfRepls == 0 {
		return nil, stacktrace.NewError("No repl with GUID '%v' in enclave with ID '%v' was found", replGuid, enclaveId)
	}
	if numOfRepls > 1 {
		return nil, stacktrace.NewError("Expected to find only one repl with GUID '%v' in enclave with ID '%v', but '%v' was found", replGuid, enclaveId, numOfRepls)
	}

	var replContainerId string
	for containerId:= range repls {
		replContainerId = containerId
	}

	hijackedResponse, err := backendCore.dockerManager.AttachToContainer(ctx, replContainerId)
	if err != nil {
		return nil, stacktrace.Propagate(err, "Couldn't attack to the REPL container")
	}

	newShell := shell.NewShell(hijackedResponse.Conn, hijackedResponse.Reader)

	return newShell, nil
}

func (backendCore *DockerKurtosisBackend) GetRepls(
	ctx context.Context,
	filters *repl.ReplFilters,
)(
	map[repl.ReplGUID]*repl.Repl,
	error,
){
	repls, err := backendCore.getMatchingRepls(ctx, filters)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting repls matching filters '%+v'", filters)
	}

	successfulRepls := map[repl.ReplGUID]*repl.Repl{}
	for _, repl := range repls {
		successfulRepls[repl.GetGUID()] = repl
	}

	return successfulRepls, nil
}

// ====================================================================================================
//                                     Private Helper Methods
// ====================================================================================================
func getReplGUID() repl.ReplGUID {
	now := time.Now()
	// TODO make this UnixNano to reduce risk of collisions???
	nowUnixSecs := now.Unix()
	replGuidStr :=  strconv.FormatInt(nowUnixSecs, guidBase)
	replGuid := repl.ReplGUID(replGuidStr)
	return replGuid
}

func (backendCore *DockerKurtosisBackend) getMatchingRepls(
	ctx context.Context,
	filters *repl.ReplFilters,
) (map[string]*repl.Repl, error) {

	searchLabels := map[string]string{
		label_key_consts.AppIDLabelKey.GetString():         label_value_consts.AppIDLabelValue.GetString(),
		label_key_consts.ContainerTypeLabelKey.GetString(): label_value_consts.InteractiveREPLContainerTypeLabelValue.GetString(),
	}
	matchingContainers, err := backendCore.dockerManager.GetContainersByLabels(ctx, searchLabels, shouldFetchAllContainersWhenRetrievingContainers)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred fetching containers using labels: %+v", searchLabels)
	}

	matchingObjects := map[string]*repl.Repl{}
	for _, container := range matchingContainers {
		containerId := container.GetId()
		object, err := getReplObjectFromContainerInfo(
			container.GetLabels(),
			container.GetStatus(),
		)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred converting container with ID '%v' into a repl object", container.GetId())
		}

		if filters.EnclaveIDs != nil && len(filters.EnclaveIDs) > 0 {
			if _, found := filters.EnclaveIDs[object.GetEnclaveID()]; !found {
				continue
			}
		}

		if filters.GUIDs != nil && len(filters.GUIDs) > 0 {
			if _, found := filters.GUIDs[object.GetGUID()]; !found {
				continue
			}
		}

		if filters.Statuses != nil && len(filters.Statuses) > 0 {
			if _, found := filters.Statuses[object.GetStatus()]; !found {
				continue
			}
		}

		matchingObjects[containerId] = object
	}

	return matchingObjects, nil
}

func getReplObjectFromContainerInfo(
	labels map[string]string,
	containerStatus types.ContainerStatus,
) (*repl.Repl, error) {

	enclaveId, found := labels[label_key_consts.EnclaveIDLabelKey.GetString()]
	if !found {
		return nil, stacktrace.NewError("Expected the repl's enclave ID to be found under label '%v' but the label wasn't present", label_key_consts.EnclaveIDLabelKey.GetString())
	}

	guid, found := labels[label_key_consts.GUIDLabelKey.GetString()]
	if !found {
		return nil, stacktrace.NewError("Expected to find repl GUID label key '%v' but none was found", label_key_consts.GUIDLabelKey.GetString())
	}

	isContainerRunning, found := isContainerRunningDeterminer[containerStatus]
	if !found {
		// This should never happen because we enforce completeness in a unit test
		return nil, stacktrace.NewError("No is-running designation found for repl container status '%v'; this is a bug in Kurtosis!", containerStatus.String())
	}
	var status container_status.ContainerStatus
	if isContainerRunning {
		status = container_status.ContainerStatus_Running
	} else {
		status = container_status.ContainerStatus_Stopped
	}

	newObject := repl.NewRepl(
		repl.ReplGUID(guid),
		enclave.EnclaveID(enclaveId),
		status,
	)

	return newObject, nil
}

// TODO AttachToRepl

// TODO GetRepls

// TODO StopRepl

// TODO DestroyRepl

// TODO RunReplExecCommand