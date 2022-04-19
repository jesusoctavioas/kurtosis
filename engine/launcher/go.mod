module github.com/kurtosis-tech/kurtosis-engine-server/launcher

go 1.15
replace (
	github.com/kurtosis-tech/container-engine-lib => ../../container-engine-lib
)
require (
	github.com/kurtosis-tech/container-engine-lib v0.0.0-20220413235819-268107a956d0
	github.com/kurtosis-tech/stacktrace v0.0.0-20211028211901-1c67a77b5409
	github.com/sirupsen/logrus v1.8.1
)
