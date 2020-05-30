package initializer

import (
	"context"
	"fmt"
	"github.com/docker/distribution/uuid"
	"github.com/docker/docker/client"
	"github.com/kurtosis-tech/kurtosis/commons/docker"
	"github.com/kurtosis-tech/kurtosis/commons/testnet"
	"github.com/kurtosis-tech/kurtosis/commons/testsuite"
	"github.com/palantir/stacktrace"
	"github.com/sirupsen/logrus"
	"io/ioutil"
)


type TestSuiteRunner struct {
	testSuite testsuite.TestSuite
	testImageName string
	testControllerImageName string
	startPortRange int
	endPortRange int
}

const (
	DEFAULT_SUBNET_MASK = "172.23.0.0/16"

	CONTAINER_NETWORK_INFO_VOLUME_MOUNTPATH = "/data/network"

	// These are an "API" of sorts - environment variables that are agreed to be set in the test controller's Docker environment
	TEST_NAME_BASH_ARG = "TEST_NAME"
	NETWORK_FILEPATH_ARG = "NETWORK_DATA_FILEPATH"
)


func NewTestSuiteRunner(
			testSuite testsuite.TestSuite,
			testImageName string,
			testControllerImageName string,
			startPortRange int,
			endPortRange int) *TestSuiteRunner {
	return &TestSuiteRunner{
		testSuite:               testSuite,
		testImageName:           testImageName,
		testControllerImageName: testControllerImageName,
		startPortRange:          startPortRange,
		endPortRange:            endPortRange,
	}
}

// Runs the tests whose names are defined in the given map (the map value is ignored - this is a hacky way to
// do a set implementation)
func (runner TestSuiteRunner) RunTests() (err error) {
	// Initialize default environment context.
	dockerCtx := context.Background()
	// Initialize a Docker client
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return stacktrace.Propagate(err,"Failed to initialize Docker client from environment.")
	}

	dockerManager, err := docker.NewDockerManager(dockerCtx, dockerClient, runner.startPortRange, runner.endPortRange)
	if err != nil {
		return stacktrace.Propagate(err, "Error in initializing Docker Manager.")
	}

	tests := runner.testSuite.GetTests()

	// TODO TODO TODO Support creating one network per testnet
	_, err = dockerManager.CreateNetwork(DEFAULT_SUBNET_MASK)
	if err != nil {
		return stacktrace.Propagate(err, "Error in creating docker subnet for testnet.")
	}

	// TODO implement parallelism and specific test selection here
	for testName, config := range tests {
		networkLoader := config.NetworkLoader
		testNetworkCfg, err := networkLoader.GetNetworkConfig(runner.testImageName)
		if err != nil {
			stacktrace.Propagate(err, "Unable to get network config from config provider")
		}

		testInstanceUuid := uuid.Generate()
		// TODO push the network name generation lower??
		networkName := fmt.Sprintf("%v-%v", testName, testInstanceUuid.String())
		publicIpProvider, err := testnet.NewFreeIpAddrTracker(networkName, DEFAULT_SUBNET_MASK)
		if err != nil {
			return stacktrace.Propagate(err, "")
		}
		serviceNetwork, err := testNetworkCfg.CreateAndRun(publicIpProvider, dockerManager)
		// TODO if we get an err back, we need to shut down whatever containers were started
		if err != nil {
			return stacktrace.Propagate(err, "Unable to create network for test '%v'", testName)
		}

		runControllerContainer(dockerManager, runner.testControllerImageName, publicIpProvider, testName, testInstanceUuid)

		// TODO gracefully shut down all the service containers we started
		for _, containerId := range serviceNetwork.ContainerIds {
			logrus.Infof("Waiting for containerId %v", containerId)
			dockerManager.WaitAndGrabLogsOnExit(containerId)
		}

	}
	return nil
}

// ======================== Private helper functions =====================================



func runControllerContainer(
		manager *docker.DockerManager,
		dockerImage string,
		ipProvider *testnet.FreeIpAddrTracker,
		testName string,
		testInstanceUuid uuid.UUID) (err error){

	volumeName := fmt.Sprintf("%v-%v", testName, testInstanceUuid.String())
	if err != nil {
		// TODO we still need to shut down the service network if we get an error here!
		return stacktrace.Propagate(err, "Could not get IP address for controller")
	}

	mountpathOnHost, err := manager.CreateVolume(volumeName)
	if err != nil {
		return stacktrace.Propagate(err, "Could not create volume to pass network info to test controller")
	}

	// TODO just for testing
	filepath := mountpathOnHost + "/testing.txt"
	err = ioutil.WriteFile(filepath, []byte("JSON data would go here"), 0644)
	if err != nil {
		return stacktrace.Propagate(err, "Could not write data to testing file")
	}

	envVariables := map[string]string{
		TEST_NAME_BASH_ARG: testName,
		// TODO just for testing
		NETWORK_FILEPATH_ARG: CONTAINER_NETWORK_INFO_VOLUME_MOUNTPATH + "/testing.txt",
	}

	ipAddr, err := ipProvider.GetFreeIpAddr()
	if err != nil {
		return stacktrace.Propagate(err, "Could not get free IP address to assign the test controller")
	}

	_, controllerContainerId, err := manager.CreateAndStartContainer(
		dockerImage,
		ipAddr,
		make(map[int]bool),
		nil,
		envVariables,
		map[string]string{
			volumeName: CONTAINER_NETWORK_INFO_VOLUME_MOUNTPATH,
		})

	// TODO add a timeout here if the test doesn't complete successfully
	manager.WaitAndGrabLogsOnExit(controllerContainerId)

	// TODO clean up the volume we created

	return nil
}
