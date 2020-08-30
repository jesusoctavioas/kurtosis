/*
 * Copyright (c) 2020 - present Kurtosis Technologies LLC.
 * All Rights Reserved.
 */

package api_container_env_vars

// Constants for the environment variables that are used in the Dockerfile, made available to Go code
const (
	// TODO sort these alphabetically
	TestSuiteContainerIdEnvVar = "TEST_SUITE_CONTAINER_ID"
	TestVolumeName = "TEST_VOLUME"
	NetworkIdEnvVar            = "NETWORK_ID"
	SubnetMaskEnvVar           = "SUBNET_MASK"
	GatewayIpEnvVar            = "GATEWAY_IP"
	LogLevelEnvVar             = "LOG_LEVEL"
	ApiLogFilepathEnvVar       = "LOG_FILEPATH"
	ApiContainerIpAddrEnvVar   = "API_CONTAINER_IP"
	TestSuiteContainerIpAddrEnvVar = "TEST_SUITE_CONTAINER_IP"
)
