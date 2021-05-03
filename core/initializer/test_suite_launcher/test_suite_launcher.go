/*
 * Copyright (c) 2021 - present Kurtosis Technologies LLC.
 * All Rights Reserved.
 */

package test_suite_launcher

import (
	"context"
	"fmt"
	"github.com/docker/go-connections/nat"
	"github.com/kurtosis-tech/kurtosis/api_container/server/api_container_server_consts"
	"github.com/kurtosis-tech/kurtosis/commons/docker_constants"
	"github.com/kurtosis-tech/kurtosis/commons/docker_manager"
	"github.com/kurtosis-tech/kurtosis/commons/free_host_port_binding_supplier"
	"github.com/kurtosis-tech/kurtosis/test_suite/test_suite_docker_consts/test_suite_container_mountpoints"
	"github.com/kurtosis-tech/kurtosis/test_suite/test_suite_docker_consts/test_suite_env_vars"
	"github.com/palantir/stacktrace"
	"github.com/sirupsen/logrus"
	"net"
	"strconv"
)

const (
	bridgeNetworkName = "bridge"

	// When a debugger is used inside a testsuite, these are the port & protocol that the debugger inside the container
	// will be told to listen on
	portForDebuggersRunningOnTestsuite     = 2778
	protocolForDebuggersRunningOnTestsuite = "tcp"

	// Can make these configurable if needed
	hostPortTrackerInterfaceIp = "127.0.0.1"
	hostPortTrackerStartRange = 8000
	hostPortTrackerEndRange = 10000

	// Identifier included in hostnames of containers running for metadata acquisition
	metadataAcquisitionContainerNameLabel = "metadata-acquisition"

	testsuiteContainerNameSuffix = "testsuite"
)

type TestsuiteContainerLauncher struct {
	executionInstanceUuid string

	suiteExecutionVolName string

	kurtosisApiImage string

	kurtosisApiLogLevel logrus.Level

	testsuiteImage string

	// The log level string that will be passed as-is to the testsuite (should be meaningful to the testsuite)
	suiteLogLevel string

	// The JSON-serialized custom params object that will be passed as-is to the testsuite
	customParamsJson string

	// This is the port object inside the testsuite container that a debugger might be listening on, if any is running at all
	// We'll always bind this port on the testsuite container to a port on the user's machine, so they can attach
	//  a debugger if desired
	debuggerPortObj nat.Port

	// Supplier for getting free host ports, which will only be non-nil if doDebuggerHostPortBinding is set to true in
	//  the constructor
	hostPortBindingSupplier *free_host_port_binding_supplier.FreeHostPortBindingSupplier
}

func NewTestsuiteContainerLauncher(
		executionInstanceUuid string,
		suiteExecutionVolName string,
		kurtosisApiImage string,
		kurtosisApiLogLevel logrus.Level,
		testsuiteImage string,
		testsuiteLogLevel string,
		customParamsJson string,
		doDebuggerHostPortBinding bool) (*TestsuiteContainerLauncher, error) {
	var hostPortBindingSupplier *free_host_port_binding_supplier.FreeHostPortBindingSupplier = nil
	if doDebuggerHostPortBinding {
		supplier, err := free_host_port_binding_supplier.NewFreeHostPortBindingSupplier(
			docker_constants.HostMachineDomainInsideContainer,
			hostPortTrackerInterfaceIp,
			protocolForDebuggersRunningOnTestsuite,
			hostPortTrackerStartRange,
			hostPortTrackerEndRange,
			map[uint32]bool{}, // We don't know of any taken ports at this point
		)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred instantiating the free host port binding supplier")
		}
		hostPortBindingSupplier = supplier
	}
	return &TestsuiteContainerLauncher{
		executionInstanceUuid:   executionInstanceUuid,
		suiteExecutionVolName:   suiteExecutionVolName,
		kurtosisApiImage:        kurtosisApiImage,
		kurtosisApiLogLevel:     kurtosisApiLogLevel,
		testsuiteImage:          testsuiteImage,
		suiteLogLevel:           testsuiteLogLevel,
		customParamsJson:        customParamsJson,
		hostPortBindingSupplier: hostPortBindingSupplier,
	}, nil
}

/*
Launches a new testsuite container for providing testsuite metadata to the initializer
*/
func (launcher TestsuiteContainerLauncher) LaunchMetadataAcquiringContainer(
		ctx context.Context,
		log *logrus.Logger,
		dockerManager *docker_manager.DockerManager) (containerId string, containerIpAddr string, err error) {
	functionCompletedSuccessfully := false

	bridgeNetworkIds, err := dockerManager.GetNetworkIdsByName(ctx, bridgeNetworkName)
	if err != nil {
		return "", "", stacktrace.Propagate(
			err,
			"An error occurred getting the network IDs matching the '%v' network",
			bridgeNetworkName)
	}
	if len(bridgeNetworkIds) == 0 || len(bridgeNetworkIds) > 1 {
		return "", "", stacktrace.NewError(
			"%v Docker network IDs were returned for the '%v' network - this is very strange!",
			len(bridgeNetworkIds),
			bridgeNetworkName)
	}
	bridgeNetworkId := bridgeNetworkIds[0]

	testsuiteEnvVars, err := launcher.generateMetadataProvidingEnvVars()
	if err != nil {
		return "", "", stacktrace.Propagate(err, "An error occurred generating the testsuite container env vars")
	}

	suiteContainerDesc := "metadata-providing testsuite container"
	log.Infof("Launching %v...", suiteContainerDesc)
	testsuiteContainerNameElems := []string{
		launcher.executionInstanceUuid,
		metadataAcquisitionContainerNameLabel,
		testsuiteContainerNameSuffix,
	}
	testsuiteContainerId, debuggerPortHostBinding, err := launcher.createAndStartTestsuiteContainerWithDebuggingPortIfNecessary(
		ctx,
		dockerManager,
		testsuiteContainerNameElems,
		bridgeNetworkId,
		nil,   // Nil because the bridge network will assign IPs on its own (and won't know what IPs are already used)
		testsuiteEnvVars,
	)
	if err != nil {
		return "", "", stacktrace.Propagate(err, "An error occurred launching the testsuite container to provide metadata to the Kurtosis API container")
	}
	defer killContainerIfNotFunctionSuccess(
		ctx,
		log,
		dockerManager,
		testsuiteContainerId,
		func() bool { return functionCompletedSuccessfully},
	)
	logSuccessfulSuiteContainerLaunch(log, suiteContainerDesc, debuggerPortHostBinding)

	ipAddr, err := dockerManager.GetContainerIP(ctx, bridgeNetworkName, testsuiteContainerId)
	if err != nil {
		return "", "", stacktrace.Propagate(err, "An error occurred getting the metadata-providing testsuite IP addr on network '%v'", bridgeNetworkName)
	}

	functionCompletedSuccessfully = true
	return testsuiteContainerId, ipAddr, nil
}

/*
Launches a new testsuite container to run a test
*/
func (launcher TestsuiteContainerLauncher) LaunchTestRunningContainer(
		ctx context.Context,
		log *logrus.Logger,
		dockerManager *docker_manager.DockerManager,
		networkId string,
		testName string,
		kurtosisApiContainerIp net.IP,
		testsuiteContainerIp net.IP) (containerId string, resultErr error){
	log.Debugf(
		"Test suite container IP: %v; kurtosis API container IP: %v",
		testsuiteContainerIp.String(),
		kurtosisApiContainerIp.String())

	functionCompletedSuccessfully := false

	testSuiteEnvVars, err := launcher.generateTestRunningEnvVars(kurtosisApiContainerIp.String(), testName)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred generating the test-running testsuite container env vars")
	}

	suiteContainerDesc := "test-running testsuite container"
	log.Infof("Launching %v....", suiteContainerDesc)
	suiteContainerNameElems := []string{
		launcher.executionInstanceUuid,
		testName,
		testsuiteContainerNameSuffix,
	}
	suiteContainerId, debuggerPortHostBinding, err := launcher.createAndStartTestsuiteContainerWithDebuggingPortIfNecessary(
		ctx,
		dockerManager,
		suiteContainerNameElems,
		networkId,
		testsuiteContainerIp,
		testSuiteEnvVars)
	if err != nil {
		return "", stacktrace.Propagate(err, "An error occurred creating the test-running testsuite container")
	}
	defer killContainerIfNotFunctionSuccess(
		ctx,
		log,
		dockerManager,
		suiteContainerId,
		func() bool { return functionCompletedSuccessfully },
	)
	logSuccessfulSuiteContainerLaunch(log, suiteContainerDesc, debuggerPortHostBinding)

	functionCompletedSuccessfully = true
	return suiteContainerId, nil
}

// ===============================================================================================
//                                 Private helper functions
// ===============================================================================================
func (launcher TestsuiteContainerLauncher) createAndStartTestsuiteContainerWithDebuggingPortIfNecessary(
		ctx context.Context,
		dockerManager *docker_manager.DockerManager,
		containerNameElems []string,
		networkId string,
		containerIpAddr net.IP,
		envVars map[string]string,
	) (containerId string, debuggingPortBindingOnHost *nat.PortBinding, resultErr error) {

	hostPortBindings := map[nat.Port]*nat.PortBinding{}
	var debuggerPortBinding *nat.PortBinding = nil
	if launcher.hostPortBindingSupplier != nil {
		freePortBinding, err := launcher.hostPortBindingSupplier.GetFreePortBinding()
		if err != nil {
			return "", nil, stacktrace.Propagate(err, "An error occurred getting a free host port binding for the testsuite container")
		}
		debuggerPortBinding = &freePortBinding

		debuggerPortInsideTestsuite, err := nat.NewPort(protocolForDebuggersRunningOnTestsuite, strconv.Itoa(portForDebuggersRunningOnTestsuite))
		if err != nil {
			return "", nil, stacktrace.Propagate(
				err,
				"An error occurred creating the port object on '%v/%v' to represent the debugger's port inside the testsuite",
				protocolForDebuggersRunningOnTestsuite,
				portForDebuggersRunningOnTestsuite,
			)
		}

		hostPortBindings[debuggerPortInsideTestsuite] = debuggerPortBinding
	}

	containerId, err := dockerManager.CreateAndStartContainer(
		ctx,
		launcher.testsuiteImage,
		containerNameElems,
		networkId,
		containerIpAddr,
		map[docker_manager.ContainerCapability]bool{}, 	// No extra capabilities needed for testsuite containers
		docker_manager.DefaultNetworkMode,  			// No special networking modes for testsuite containers
		hostPortBindings,
		nil, // Nil ENTRYPOINT args because we expect the test suite image to be parameterized with variables
		nil, // Nil CMD args because we expect the test suite image to be parameterized with variables
		envVars,
		map[string]string{}, 		// No bind mounts for a testsuite container
		map[string]string{
			launcher.suiteExecutionVolName: test_suite_container_mountpoints.TestsuiteContainerSuiteExVolMountpoint,
		},
		false, // The testsuite container should never be able to access the machine hosting Docker
	)
	if err != nil {
		return "", nil, stacktrace.Propagate(err, "An error occurred creating and starting the testsuite container")
	}

	return containerId, debuggerPortBinding, nil
}

func logSuccessfulSuiteContainerLaunch(
		log *logrus.Logger,
		suiteContainerDesc string,
		debuggerPortHostBinding *nat.PortBinding) {
	suiteLaunchSupplementalLogInfo := ""
	if debuggerPortHostBinding != nil {
		suiteLaunchSupplementalLogInfo = fmt.Sprintf(
			" with debugger port bound to host port %v:%v (if a debugger is running, you may need to connect to this port to proceed)",
			debuggerPortHostBinding.HostIP,
			debuggerPortHostBinding.HostPort,
		)
	}
	log.Infof("Successfully created %v%v", suiteContainerDesc, suiteLaunchSupplementalLogInfo)

}

func (launcher TestsuiteContainerLauncher) generateMetadataProvidingEnvVars() (map[string]string, error) {
	result, err := launcher.generateTestSuiteEnvVars(test_suite_env_vars.MetadataProvidingMode, "", "")
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred generating the metadata-providing env vars")
	}
	return result, nil
}

func (launcher TestsuiteContainerLauncher) generateTestRunningEnvVars(
		kurtosisApiIp string,
		testName string) (map[string]string, error) {
	result, err := launcher.generateTestSuiteEnvVars(test_suite_env_vars.TestRunningMode, kurtosisApiIp, testName)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred generating the test-running env vars")
	}
	return result, nil
}

/*
Generates the map of environment variables needed to run a test suite container
*/
func (launcher TestsuiteContainerLauncher) generateTestSuiteEnvVars(
		mode test_suite_env_vars.TestSuiteMode,
		kurtosisApiIp string,
		testName string) (map[string]string, error) {
	debuggerPortIntStr := strconv.Itoa(launcher.debuggerPortObj.Int())
	kurtosisApiSocket := ""
	if mode == test_suite_env_vars.TestRunningMode {
		kurtosisApiSocket = fmt.Sprintf("%v:%v", kurtosisApiIp, api_container_server_consts.ListenPort)
	}
	// TODO switch to the envVars requiring a visitor to hit, so we get them all
	standardVars := map[string]string{
		test_suite_env_vars.CustomParamsJson:        launcher.customParamsJson,
		test_suite_env_vars.DebuggerPortEnvVar:      debuggerPortIntStr,
		test_suite_env_vars.KurtosisApiSocketEnvVar: kurtosisApiSocket,
		test_suite_env_vars.LogLevelEnvVar:          launcher.suiteLogLevel,
		test_suite_env_vars.ModeEnvVar: 			 string(mode),
		test_suite_env_vars.TestNameEnvVar:          testName,
	}
	return standardVars, nil
}

// This function is intended to be run as a deferred function, to kill a container that was started if the
//  outer function exits with an error
func killContainerIfNotFunctionSuccess(
		ctx context.Context,
		log *logrus.Logger,
		dockerManager *docker_manager.DockerManager,
		containerId string,
		// This needs to be a function so that it gets evaluated at time of killContainer... function
		didFunctionCompleteSuccessfully func() bool) {
	if !didFunctionCompleteSuccessfully() {
		if err := dockerManager.KillContainer(ctx, containerId); err != nil {
			log.Errorf("A container was started but the function that started it exited with an error. The container needed " +
				"to be stopped to avoid leaking a running container, but the following error occurred when attempting to stop the " +
				"container:")
			fmt.Fprintln(log.Out, err)
			log.Errorf("ACTION REQUIRED: You will need to stop the testsuite container with ID '%v' manually!", containerId)
		}
	} else {
		log.Debugf("Skipping killing container '%v' because function completed successfully", containerId)
	}
}
